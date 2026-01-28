package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/missdeer/aiproxy/balancer"
	"github.com/missdeer/aiproxy/config"
)

// GeminiHandler handles Gemini API requests
type GeminiHandler struct {
	cfg      *config.Config
	balancer *balancer.WeightedRoundRobin
	client   *http.Client
	mu       sync.RWMutex
}

// NewGeminiHandler creates a new Gemini API handler
func NewGeminiHandler(cfg *config.Config) *GeminiHandler {
	return &GeminiHandler{
		cfg:      cfg,
		balancer: balancer.NewWeightedRoundRobin(cfg.Upstreams),
		client:   &http.Client{},
	}
}

// UpdateConfig updates the handler's configuration
func (h *GeminiHandler) UpdateConfig(cfg *config.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
	h.balancer.Update(cfg.Upstreams)
}

func (h *GeminiHandler) defaultMaxTokens() int {
	h.mu.RLock()
	cfg := h.cfg
	h.mu.RUnlock()
	if cfg == nil || cfg.DefaultMaxTokens <= 0 {
		return 4096
	}
	return cfg.DefaultMaxTokens
}

// Gemini API types
type GeminiRequest struct {
	Contents          []GeminiContent  `json:"contents"`
	SystemInstruction *GeminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *GeminiGenConfig `json:"generationConfig,omitempty"`
}

type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts"`
}

type GeminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *GeminiInlineData `json:"inlineData,omitempty"`
}

type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type GeminiGenConfig struct {
	Temperature     float64  `json:"temperature,omitempty"`
	TopP            float64  `json:"topP,omitempty"`
	TopK            int      `json:"topK,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type GeminiResponse struct {
	Candidates    []GeminiCandidate `json:"candidates"`
	UsageMetadata *GeminiUsage      `json:"usageMetadata,omitempty"`
	ModelVersion  string            `json:"modelVersion,omitempty"`
}

type GeminiCandidate struct {
	Content       GeminiContent `json:"content"`
	FinishReason  string        `json:"finishReason,omitempty"`
	Index         int           `json:"index"`
	SafetyRatings []any         `json:"safetyRatings,omitempty"`
}

type GeminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// ServeHTTP handles requests to /v1/gemini/* or /v1beta/models/*
func (h *GeminiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract model from URL path
	// Expected: /v1beta/models/{model}:generateContent or /v1beta/models/{model}:streamGenerateContent
	path := r.URL.Path
	model, isStream := parseGeminiPath(path)
	if model == "" {
		http.Error(w, "Invalid path format", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var req GeminiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[ERROR] Invalid JSON request: %v", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Extract prompt preview for logging
	promptPreview := extractGeminiPromptPreview(req.Contents)
	log.Printf("[GEMINI REQUEST] Model: %s, Stream: %v, Prompt: %s", model, isStream, promptPreview)

	originalModel := model

	upstreams := h.balancer.GetAll()
	if len(upstreams) == 0 {
		log.Printf("[ERROR] No upstreams configured")
		http.Error(w, "No upstreams configured", http.StatusServiceUnavailable)
		return
	}

	// Filter upstreams that support the requested model and are available
	var supportedUpstreams []config.Upstream
	for _, u := range upstreams {
		if u.SupportsModel(originalModel) && h.balancer.IsAvailable(u.Name) {
			supportedUpstreams = append(supportedUpstreams, u)
		}
	}

	if len(supportedUpstreams) == 0 {
		log.Printf("[ERROR] No available upstream supports model: %s", originalModel)
		h.writeGeminiError(w, http.StatusBadRequest, "MODEL_NOT_FOUND", fmt.Sprintf("No upstream supports model: %s", originalModel))
		return
	}

	log.Printf("[INFO] Found %d available upstreams for model %s", len(supportedUpstreams), originalModel)

	// Use NextForModel to get the next upstream that supports this model
	next := h.balancer.NextForModel(originalModel)

	// Reorder upstreams: start from the one returned by NextForModel
	startIdx := 0
	if next != nil {
		for i, u := range supportedUpstreams {
			if u.Name == next.Name {
				startIdx = i
				break
			}
		}
	}
	ordered := make([]config.Upstream, 0, len(supportedUpstreams))
	ordered = append(ordered, supportedUpstreams[startIdx:]...)
	ordered = append(ordered, supportedUpstreams[:startIdx]...)

	var lastErr error
	var lastStatus int

	for _, upstream := range ordered {
		mappedModel := upstream.MapModel(originalModel)
		log.Printf("[FORWARD] Upstream: %s, URL: %s, Model: %s -> %s, APIType: %s",
			upstream.Name, upstream.BaseURL, originalModel, mappedModel, upstream.GetAPIType())

		status, respBody, respHeaders, err := h.forwardRequest(upstream, mappedModel, body, isStream, r)
		if err != nil {
			log.Printf("[ERROR] Upstream %s connection error: %v", upstream.Name, err)
			if h.balancer.RecordFailure(upstream.Name) {
				log.Printf("[CIRCUIT] Upstream %s marked as unavailable after %d consecutive failures", upstream.Name, 3)
			}
			lastErr = err
			lastStatus = http.StatusBadGateway
			continue
		}

		if status >= 400 {
			log.Printf("[ERROR] Upstream %s returned HTTP %d, response: %s", upstream.Name, status, truncateString(string(respBody), 200))
			if h.balancer.RecordFailure(upstream.Name) {
				log.Printf("[CIRCUIT] Upstream %s marked as unavailable after %d consecutive failures", upstream.Name, 3)
			}
			lastErr = fmt.Errorf("upstream returned status %d", status)
			lastStatus = status
			continue
		}

		// Success
		h.balancer.RecordSuccess(upstream.Name)
		log.Printf("[SUCCESS] Upstream %s returned HTTP %d", upstream.Name, status)

		for k, vv := range respHeaders {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)

		if isStream {
			h.streamResponse(w, respBody)
		} else {
			w.Write(respBody)
		}
		return
	}

	log.Printf("[FAILED] All %d upstreams failed for model %s", len(ordered), originalModel)
	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	h.writeGeminiError(w, lastStatus, "UPSTREAM_ERROR", fmt.Sprintf("All upstreams failed: %v", lastErr))
}

func parseGeminiPath(path string) (model string, isStream bool) {
	// /v1beta/models/gemini-pro:generateContent
	// /v1beta/models/gemini-pro:streamGenerateContent
	path = strings.TrimPrefix(path, "/v1beta/models/")
	path = strings.TrimPrefix(path, "/v1/models/")

	if strings.HasSuffix(path, ":streamGenerateContent") {
		model = strings.TrimSuffix(path, ":streamGenerateContent")
		isStream = true
	} else if strings.HasSuffix(path, ":generateContent") {
		model = strings.TrimSuffix(path, ":generateContent")
		isStream = false
	}
	return
}

func (h *GeminiHandler) forwardRequest(upstream config.Upstream, model string, originalBody []byte, isStream bool, originalReq *http.Request) (int, []byte, http.Header, error) {
	var bodyMap map[string]any
	if err := json.Unmarshal(originalBody, &bodyMap); err != nil {
		return 0, nil, nil, err
	}

	apiType := upstream.GetAPIType()
	defaultMaxTokens := h.defaultMaxTokens()

	var url string
	var modifiedBody []byte
	var err error

	switch apiType {
	case config.APITypeGemini:
		// Native Gemini upstream
		action := "generateContent"
		if isStream {
			action = "streamGenerateContent"
		}
		url = fmt.Sprintf("%s/v1beta/models/%s:%s", strings.TrimSuffix(upstream.BaseURL, "/"), model, action)
		// Pass through query parameters when API types match (excluding key parameter which is handled separately)
		if originalReq.URL.RawQuery != "" {
			url = url + "?" + originalReq.URL.RawQuery
		}
		modifiedBody = originalBody

	case config.APITypeAnthropic:
		// Convert to Anthropic format
		bodyMap = convertGeminiToAnthropicRequest(bodyMap, defaultMaxTokens)
		bodyMap["model"] = model
		if isStream {
			bodyMap["stream"] = true
		}
		modifiedBody, err = json.Marshal(bodyMap)
		if err != nil {
			return 0, nil, nil, err
		}
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/messages"

	case config.APITypeOpenAI:
		// Convert to OpenAI format
		bodyMap = convertGeminiToOpenAIRequest(bodyMap)
		bodyMap["model"] = model
		if isStream {
			bodyMap["stream"] = true
		}
		modifiedBody, err = json.Marshal(bodyMap)
		if err != nil {
			return 0, nil, nil, err
		}
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/chat/completions"

	case config.APITypeResponses:
		// Convert to Responses format
		bodyMap = convertGeminiToResponsesRequest(bodyMap)
		bodyMap["model"] = model
		if isStream {
			bodyMap["stream"] = true
		}
		modifiedBody, err = json.Marshal(bodyMap)
		if err != nil {
			return 0, nil, nil, err
		}
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/responses"

	default:
		return 0, nil, nil, fmt.Errorf("unsupported API type: %s", apiType)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(modifiedBody))
	if err != nil {
		return 0, nil, nil, err
	}

	// Copy headers
	for k, vv := range originalReq.Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	stripHopByHopHeaders(req.Header)

	// Set authentication
	switch apiType {
	case config.APITypeGemini:
		// Gemini uses API key in URL or header
		if !strings.Contains(url, "key=") {
			url = url + "?key=" + upstream.Token
			req.URL, _ = req.URL.Parse(url)
		}
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	case config.APITypeAnthropic:
		req.Header.Set("x-api-key", upstream.Token)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Del("Authorization")
	case config.APITypeOpenAI, config.APITypeResponses:
		req.Header.Set("Authorization", "Bearer "+upstream.Token)
		req.Header.Del("x-api-key")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Del("Content-Length")

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	// Handle response conversion
	if resp.StatusCode < 400 && apiType != config.APITypeGemini {
		if isStream {
			geminiResp, err := h.convertStreamToGemini(resp.Body, apiType)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("failed to convert stream: %w", err)
			}
			headers := resp.Header.Clone()
			stripHopByHopHeaders(headers)
			headers.Del("Content-Length")
			headers.Set("Content-Type", "application/json")
			return resp.StatusCode, geminiResp, headers, nil
		} else {
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return 0, nil, nil, err
			}
			geminiResp, err := convertToGeminiResponse(respBody, apiType)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("failed to convert response: %w", err)
			}
			headers := resp.Header.Clone()
			stripHopByHopHeaders(headers)
			headers.Del("Content-Length")
			headers.Set("Content-Type", "application/json")
			return resp.StatusCode, geminiResp, headers, nil
		}
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}

	return resp.StatusCode, respBody, resp.Header, nil
}

func (h *GeminiHandler) streamResponse(w http.ResponseWriter, body []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Write(body)
		return
	}
	w.Write(body)
	flusher.Flush()
}

func (h *GeminiHandler) writeGeminiError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": message,
			"status":  code,
		},
	})
}

func extractGeminiPromptPreview(contents []GeminiContent) string {
	for i := len(contents) - 1; i >= 0; i-- {
		if contents[i].Role == "user" || contents[i].Role == "" {
			for _, part := range contents[i].Parts {
				if part.Text != "" {
					return truncateString(part.Text, 100)
				}
			}
		}
	}
	return "(empty)"
}

// convertGeminiToAnthropicRequest converts Gemini request to Anthropic format
func convertGeminiToAnthropicRequest(req map[string]any, defaultMaxTokens int) map[string]any {
	anthropicReq := make(map[string]any)

	// Convert contents to messages
	if contents, ok := req["contents"].([]any); ok {
		var messages []map[string]any
		for _, content := range contents {
			if contentMap, ok := content.(map[string]any); ok {
				role := "user"
				if r, ok := contentMap["role"].(string); ok {
					if r == "model" {
						role = "assistant"
					} else {
						role = r
					}
				}

				var contentParts any
				if parts, ok := contentMap["parts"].([]any); ok {
					if len(parts) == 1 {
						if part, ok := parts[0].(map[string]any); ok {
							if text, ok := part["text"].(string); ok {
								contentParts = text
							}
						}
					} else {
						var anthropicParts []map[string]any
						for _, part := range parts {
							if partMap, ok := part.(map[string]any); ok {
								if text, ok := partMap["text"].(string); ok {
									anthropicParts = append(anthropicParts, map[string]any{
										"type": "text",
										"text": text,
									})
								}
								if inlineData, ok := partMap["inlineData"].(map[string]any); ok {
									anthropicParts = append(anthropicParts, map[string]any{
										"type": "image",
										"source": map[string]any{
											"type":       "base64",
											"media_type": inlineData["mimeType"],
											"data":       inlineData["data"],
										},
									})
								}
							}
						}
						contentParts = anthropicParts
					}
				}

				messages = append(messages, map[string]any{
					"role":    role,
					"content": contentParts,
				})
			}
		}
		anthropicReq["messages"] = messages
	}

	// Convert system instruction
	if sysInstr, ok := req["systemInstruction"].(map[string]any); ok {
		if parts, ok := sysInstr["parts"].([]any); ok {
			var systemText string
			for _, part := range parts {
				if partMap, ok := part.(map[string]any); ok {
					if text, ok := partMap["text"].(string); ok {
						systemText += text
					}
				}
			}
			if systemText != "" {
				anthropicReq["system"] = systemText
			}
		}
	}

	// Convert generation config
	if genConfig, ok := req["generationConfig"].(map[string]any); ok {
		if temp, ok := genConfig["temperature"]; ok {
			anthropicReq["temperature"] = temp
		}
		if topP, ok := genConfig["topP"]; ok {
			anthropicReq["top_p"] = topP
		}
		if maxTokens, ok := genConfig["maxOutputTokens"]; ok {
			anthropicReq["max_tokens"] = maxTokens
		} else {
			anthropicReq["max_tokens"] = defaultMaxTokens
		}
		if stopSeqs, ok := genConfig["stopSequences"]; ok {
			anthropicReq["stop_sequences"] = stopSeqs
		}
	} else {
		anthropicReq["max_tokens"] = defaultMaxTokens
	}

	return anthropicReq
}

// convertGeminiToOpenAIRequest converts Gemini request to OpenAI format
func convertGeminiToOpenAIRequest(req map[string]any) map[string]any {
	openAIReq := make(map[string]any)

	var messages []map[string]any

	// Convert system instruction
	if sysInstr, ok := req["systemInstruction"].(map[string]any); ok {
		if parts, ok := sysInstr["parts"].([]any); ok {
			var systemText string
			for _, part := range parts {
				if partMap, ok := part.(map[string]any); ok {
					if text, ok := partMap["text"].(string); ok {
						systemText += text
					}
				}
			}
			if systemText != "" {
				messages = append(messages, map[string]any{
					"role":    "system",
					"content": systemText,
				})
			}
		}
	}

	// Convert contents to messages
	if contents, ok := req["contents"].([]any); ok {
		for _, content := range contents {
			if contentMap, ok := content.(map[string]any); ok {
				role := "user"
				if r, ok := contentMap["role"].(string); ok {
					if r == "model" {
						role = "assistant"
					} else {
						role = r
					}
				}

				var contentValue any
				if parts, ok := contentMap["parts"].([]any); ok {
					if len(parts) == 1 {
						if part, ok := parts[0].(map[string]any); ok {
							if text, ok := part["text"].(string); ok {
								contentValue = text
							}
						}
					} else {
						var openAIParts []map[string]any
						for _, part := range parts {
							if partMap, ok := part.(map[string]any); ok {
								if text, ok := partMap["text"].(string); ok {
									openAIParts = append(openAIParts, map[string]any{
										"type": "text",
										"text": text,
									})
								}
								if inlineData, ok := partMap["inlineData"].(map[string]any); ok {
									mimeType, _ := inlineData["mimeType"].(string)
									data, _ := inlineData["data"].(string)
									openAIParts = append(openAIParts, map[string]any{
										"type": "image_url",
										"image_url": map[string]any{
											"url": fmt.Sprintf("data:%s;base64,%s", mimeType, data),
										},
									})
								}
							}
						}
						contentValue = openAIParts
					}
				}

				messages = append(messages, map[string]any{
					"role":    role,
					"content": contentValue,
				})
			}
		}
	}

	openAIReq["messages"] = messages

	// Convert generation config
	if genConfig, ok := req["generationConfig"].(map[string]any); ok {
		if temp, ok := genConfig["temperature"]; ok {
			openAIReq["temperature"] = temp
		}
		if topP, ok := genConfig["topP"]; ok {
			openAIReq["top_p"] = topP
		}
		if maxTokens, ok := genConfig["maxOutputTokens"]; ok {
			openAIReq["max_tokens"] = maxTokens
		}
		if stopSeqs, ok := genConfig["stopSequences"]; ok {
			openAIReq["stop"] = stopSeqs
		}
	}

	return openAIReq
}

// convertToGeminiResponse converts Anthropic or OpenAI response to Gemini format
func convertToGeminiResponse(body []byte, apiType config.APIType) ([]byte, error) {
	if apiType == config.APITypeAnthropic {
		return convertAnthropicToGemini(body)
	}
	return convertOpenAIToGemini(body)
}

func convertAnthropicToGemini(body []byte) ([]byte, error) {
	var anthropicResp struct {
		ID      string `json:"id"`
		Content []any  `json:"content"`
		Model   string `json:"model"`
		Usage   struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		StopReason string `json:"stop_reason"`
	}

	if err := json.Unmarshal(body, &anthropicResp); err != nil {
		return nil, err
	}

	var parts []GeminiPart
	for _, block := range anthropicResp.Content {
		if blockMap, ok := block.(map[string]any); ok {
			if blockType, ok := blockMap["type"].(string); ok && blockType == "text" {
				if text, ok := blockMap["text"].(string); ok {
					parts = append(parts, GeminiPart{Text: text})
				}
			}
		}
	}

	resp := GeminiResponse{
		Candidates: []GeminiCandidate{
			{
				Content: GeminiContent{
					Role:  "model",
					Parts: parts,
				},
				FinishReason: mapAnthropicToGeminiFinishReason(anthropicResp.StopReason),
				Index:        0,
			},
		},
		UsageMetadata: &GeminiUsage{
			PromptTokenCount:     anthropicResp.Usage.InputTokens,
			CandidatesTokenCount: anthropicResp.Usage.OutputTokens,
			TotalTokenCount:      anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
		},
		ModelVersion: anthropicResp.Model,
	}

	return json.Marshal(resp)
}

func convertOpenAIToGemini(body []byte) ([]byte, error) {
	var openAIResp OpenAIChatResponse
	if err := json.Unmarshal(body, &openAIResp); err != nil {
		return nil, err
	}

	var parts []GeminiPart
	if len(openAIResp.Choices) > 0 && openAIResp.Choices[0].Message != nil {
		if text, ok := openAIResp.Choices[0].Message.Content.(string); ok {
			parts = append(parts, GeminiPart{Text: text})
		}
	}

	var finishReason string
	if len(openAIResp.Choices) > 0 && openAIResp.Choices[0].FinishReason != nil {
		finishReason = mapOpenAIToGeminiFinishReason(*openAIResp.Choices[0].FinishReason)
	}

	resp := GeminiResponse{
		Candidates: []GeminiCandidate{
			{
				Content: GeminiContent{
					Role:  "model",
					Parts: parts,
				},
				FinishReason: finishReason,
				Index:        0,
			},
		},
		UsageMetadata: &GeminiUsage{
			PromptTokenCount:     openAIResp.Usage.PromptTokens,
			CandidatesTokenCount: openAIResp.Usage.CompletionTokens,
			TotalTokenCount:      openAIResp.Usage.TotalTokens,
		},
		ModelVersion: openAIResp.Model,
	}

	return json.Marshal(resp)
}

func (h *GeminiHandler) convertStreamToGemini(reader io.Reader, apiType config.APIType) ([]byte, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var (
		textContent      strings.Builder
		inputTokens      int
		outputTokens     int
		model            string
		currentEventType string
		dataBuilder      strings.Builder
	)

	flushEvent := func() {
		if dataBuilder.Len() == 0 {
			return
		}
		data := strings.TrimSpace(dataBuilder.String())
		dataBuilder.Reset()

		if data == "" || data == "[DONE]" {
			return
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return
		}

		if apiType == config.APITypeAnthropic {
			eventType := currentEventType
			if eventType == "" {
				if t, ok := event["type"].(string); ok {
					eventType = t
				}
			}

			switch eventType {
			case "message_start":
				if msg, ok := event["message"].(map[string]any); ok {
					if m, ok := msg["model"].(string); ok {
						model = m
					}
					if usage, ok := msg["usage"].(map[string]any); ok {
						if input, ok := usage["input_tokens"].(float64); ok {
							inputTokens = int(input)
						}
					}
				}
			case "content_block_delta":
				if delta, ok := event["delta"].(map[string]any); ok {
					if deltaType, ok := delta["type"].(string); ok && deltaType == "text_delta" {
						if text, ok := delta["text"].(string); ok {
							textContent.WriteString(text)
						}
					}
				}
			case "message_delta":
				if usage, ok := event["usage"].(map[string]any); ok {
					if output, ok := usage["output_tokens"].(float64); ok {
						outputTokens = int(output)
					}
				}
			}
		} else {
			// OpenAI format
			if m, ok := event["model"].(string); ok {
				model = m
			}
			if choices, ok := event["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if delta, ok := choice["delta"].(map[string]any); ok {
						if content, ok := delta["content"].(string); ok {
							textContent.WriteString(content)
						}
					}
				}
			}
		}
	}

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			flushEvent()
			currentEventType = ""
			continue
		}

		if strings.HasPrefix(line, ":") {
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			if dataBuilder.Len() > 0 {
				dataBuilder.WriteByte('\n')
			}
			dataBuilder.WriteString(data)
		}
	}

	flushEvent()

	// Build Gemini response
	resp := GeminiResponse{
		Candidates: []GeminiCandidate{
			{
				Content: GeminiContent{
					Role: "model",
					Parts: []GeminiPart{
						{Text: textContent.String()},
					},
				},
				FinishReason: "STOP",
				Index:        0,
			},
		},
		UsageMetadata: &GeminiUsage{
			PromptTokenCount:     inputTokens,
			CandidatesTokenCount: outputTokens,
			TotalTokenCount:      inputTokens + outputTokens,
		},
		ModelVersion: model,
	}

	return json.Marshal(resp)
}

func mapAnthropicToGeminiFinishReason(reason string) string {
	switch reason {
	case "end_turn":
		return "STOP"
	case "max_tokens":
		return "MAX_TOKENS"
	case "stop_sequence":
		return "STOP"
	default:
		return "STOP"
	}
}

func mapOpenAIToGeminiFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "tool_calls":
		return "STOP"
	default:
		return "STOP"
	}
}

// GeminiCompatHandler wraps GeminiHandler to handle path-based routing
type GeminiCompatHandler struct {
	handler *GeminiHandler
}

func NewGeminiCompatHandler(cfg *config.Config) *GeminiCompatHandler {
	return &GeminiCompatHandler{
		handler: NewGeminiHandler(cfg),
	}
}

// UpdateConfig updates the handler's configuration
func (h *GeminiCompatHandler) UpdateConfig(cfg *config.Config) {
	h.handler.UpdateConfig(cfg)
}

func (h *GeminiCompatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}
