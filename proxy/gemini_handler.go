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
	"time"

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
	timeout := time.Duration(cfg.UpstreamRequestTimeout) * time.Second
	return &GeminiHandler{
		cfg:      cfg,
		balancer: balancer.NewWeightedRoundRobin(cfg.Upstreams),
		client:   newHTTPClient(timeout),
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
	currentModel := originalModel
	visited := map[string]bool{currentModel: true}

	var lastErr error
	var lastStatus int
	attemptedUpstream := false

	for { // outer fallback loop
		upstreams := h.balancer.GetAll()
		if len(upstreams) == 0 {
			log.Printf("[ERROR] No upstreams configured")
			http.Error(w, "No upstreams configured", http.StatusServiceUnavailable)
			return
		}

		// Filter upstreams that support the requested model and are available
		var supportedUpstreams []config.Upstream
		for _, u := range upstreams {
			if u.SupportsModel(currentModel) && h.balancer.IsAvailable(u.Name, currentModel) {
				supportedUpstreams = append(supportedUpstreams, u)
			}
		}

		if len(supportedUpstreams) == 0 {
			log.Printf("[WARN] No available upstream supports model: %s", currentModel)
			goto tryFallback
		}

		log.Printf("[INFO] Found %d available upstreams for model %s", len(supportedUpstreams), currentModel)

		{
			// Use NextForModel to get the next upstream that supports this model
			next := h.balancer.NextForModel(currentModel)

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

			for _, upstream := range ordered {
				attemptedUpstream = true
				mappedModel := upstream.MapModel(currentModel)
				log.Printf("[FORWARD] Upstream: %s, URL: %s, Model: %s -> %s, APIType: %s",
					upstream.Name, upstream.BaseURL, currentModel, mappedModel, upstream.GetAPIType())

				status, respBody, respHeaders, streamResp, err := h.forwardRequest(upstream, mappedModel, body, isStream, r)
				if err != nil {
					log.Printf("[ERROR] Upstream %s connection error: %v", upstream.Name, err)
					if h.balancer.RecordFailure(upstream.Name, currentModel) {
						log.Printf("[CIRCUIT] Upstream %s model %s marked as unavailable after %d consecutive failures", upstream.Name, currentModel, 3)
					}
					lastErr = err
					lastStatus = http.StatusBadGateway
					continue
				}

				// True streaming passthrough: pipe response directly
				if streamResp != nil {
					defer streamResp.Body.Close()
					for k, vv := range streamResp.Header {
						for _, v := range vv {
							w.Header().Add(k, v)
						}
					}
					stripHopByHopHeaders(w.Header())
					w.WriteHeader(status)
					streamStart := time.Now()
					log.Printf("[GEMINI] stream_start upstream=%s status=%d (headers sent to client)", upstream.Name, status)

					result := PipeStream(w, streamResp.Body)
					switch {
					case result.UpstreamErr != nil:
						if r.Context().Err() != nil {
							log.Printf("[INFO] Upstream %s stream aborted by client disconnect", upstream.Name)
						} else {
							log.Printf("[ERROR] Upstream %s stream read error: %v", upstream.Name, result.UpstreamErr)
							h.balancer.RecordFailure(upstream.Name, currentModel)
						}
					case result.DownstreamErr != nil:
						log.Printf("[INFO] Upstream %s stream ended by client disconnect: %v", upstream.Name, result.DownstreamErr)
					default:
						h.balancer.RecordSuccess(upstream.Name, currentModel)
						log.Printf("[SUCCESS] Upstream %s streaming completed (%d bytes, %s)", upstream.Name, result.BytesWritten, time.Since(streamStart))
					}
					return
				}

				if status >= 400 {
					log.Printf("[ERROR] Upstream %s returned HTTP %d, response: %s", upstream.Name, status, truncateString(string(respBody), 200))
					if h.balancer.RecordFailure(upstream.Name, currentModel) {
						log.Printf("[CIRCUIT] Upstream %s model %s marked as unavailable after %d consecutive failures", upstream.Name, currentModel, 3)
					}
					lastErr = fmt.Errorf("upstream returned status %d", status)
					lastStatus = status
					continue
				}

				// Success
				h.balancer.RecordSuccess(upstream.Name, currentModel)
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

			// All upstreams failed for this model
			log.Printf("[FAILED] All %d upstreams failed for model %s, last error: %v, last status: %d", len(ordered), currentModel, lastErr, lastStatus)
		}

	tryFallback:
		// Check for model fallback
		h.mu.RLock()
		fallback := h.cfg.GetModelFallback(currentModel)
		h.mu.RUnlock()
		if fallback == "" || visited[fallback] {
			break
		}
		log.Printf("[FALLBACK] model %s -> %s (all upstreams exhausted)", currentModel, fallback)
		visited[fallback] = true
		currentModel = fallback
	}

	// All upstreams and fallbacks exhausted
	if !attemptedUpstream {
		h.writeGeminiError(w, http.StatusBadRequest, "MODEL_NOT_FOUND", fmt.Sprintf("No upstream supports model: %s", originalModel))
		return
	}
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

func (h *GeminiHandler) forwardRequest(upstream config.Upstream, model string, originalBody []byte, isStream bool, originalReq *http.Request) (int, []byte, http.Header, *http.Response, error) {
	var bodyMap map[string]any
	if err := json.Unmarshal(originalBody, &bodyMap); err != nil {
		return 0, nil, nil, nil, err
	}

	apiType := upstream.GetAPIType()
	defaultMaxTokens := h.defaultMaxTokens()

	// Native Gemini - direct passthrough without format conversion
	if apiType == config.APITypeGemini {
		action := "generateContent"
		if isStream {
			action = "streamGenerateContent"
		}
		url := fmt.Sprintf("%s/v1beta/models/%s:%s", strings.TrimSuffix(upstream.BaseURL, "/"), model, action)
		rawQuery := ""
		if originalReq.URL.RawQuery != "" {
			rawQuery = originalReq.URL.RawQuery
		}

		// Streaming: return raw response for direct pipe to client
		if isStream {
			resp, err := doHTTPRequestStream(h.client, url, originalBody, upstream, config.APITypeGemini, originalReq, rawQuery)
			if err != nil {
				return 0, nil, nil, nil, err
			}
			if resp.StatusCode >= 400 {
				defer resp.Body.Close()
				errBody, _ := io.ReadAll(resp.Body)
				headers := resp.Header.Clone()
				stripHopByHopHeaders(headers)
				return resp.StatusCode, errBody, headers, nil, nil
			}
			return resp.StatusCode, nil, nil, resp, nil
		}

		// Non-streaming: use existing buffered path
		status, respBody, headers, err := doHTTPRequest(h.client, url, originalBody, upstream, config.APITypeGemini, originalReq, rawQuery)
		if err != nil {
			return 0, nil, nil, nil, err
		}
		return status, respBody, headers, nil, nil
	}

	// Convert Gemini request to canonical (Responses) format
	canonicalBody := convertGeminiToResponsesRequest(bodyMap)
	canonicalBody["model"] = model
	if isStream {
		canonicalBody["stream"] = true
	}
	// Pass through defaultMaxTokens if needed
	if _, hasMaxTokens := canonicalBody["max_output_tokens"]; !hasMaxTokens {
		canonicalBody["max_output_tokens"] = defaultMaxTokens
	}
	canonicalBytes, err := json.Marshal(canonicalBody)
	if err != nil {
		return 0, nil, nil, nil, err
	}

	// Send via OutboundSender
	sender := GetOutboundSender(apiType)

	// Streaming fast path: if sender supports SendStream and client wants streaming,
	// try to get the raw response for PipeStream passthrough.
	if isStream {
		if streamSender, ok := sender.(StreamCapableSender); ok {
			resp, respFormat, err := streamSender.SendStream(h.client, upstream, canonicalBytes, originalReq)
			if err != nil {
				return 0, nil, nil, nil, err
			}
			// Only use fast path if response format matches the client's protocol
			if respFormat == config.APITypeGemini {
				log.Printf("[GEMINI] stream_mode=format_compatible_passthrough upstream=%s", upstream.Name)
				return HandleStreamResponse(resp)
			}
			// Format mismatch: read body and fall through to conversion path
			log.Printf("[GEMINI] stream_mode=buffered_conversion upstream=%s respFormat=%s", upstream.Name, respFormat)
			defer resp.Body.Close()
			respBody, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				return 0, nil, nil, nil, readErr
			}
			headers := resp.Header.Clone()
			StripHopByHopHeaders(headers)
			headers.Del("Content-Length")
			headers.Del("Content-Encoding")
			if resp.StatusCode >= 400 {
				return resp.StatusCode, respBody, headers, nil, nil
			}
			// Fall through to convert stream response
			geminiResp, convErr := h.convertStreamToGemini(bytes.NewReader(respBody), respFormat)
			if convErr != nil {
				return 0, nil, nil, nil, fmt.Errorf("failed to convert stream response: %w", convErr)
			}
			headers.Set("Content-Type", "application/json")
			return resp.StatusCode, geminiResp, headers, nil, nil
		}
	}

	status, respBody, respHeaders, respFormat, err := sender.Send(h.client, upstream, canonicalBytes, isStream, originalReq)
	if err != nil {
		return status, nil, nil, nil, err
	}

	// Error responses pass through without conversion
	if status >= 400 {
		return status, respBody, respHeaders, nil, nil
	}

	// Convert response back to Gemini format
	if isStream {
		geminiResp, err := h.convertStreamToGemini(bytes.NewReader(respBody), respFormat)
		if err != nil {
			return 0, nil, nil, nil, fmt.Errorf("failed to convert stream response: %w", err)
		}
		respHeaders.Del("Content-Length")
		respHeaders.Set("Content-Type", "application/json")
		return status, geminiResp, respHeaders, nil, nil
	}

	geminiResp, err := convertToGeminiResponse(respBody, respFormat)
	if err != nil {
		return 0, nil, nil, nil, fmt.Errorf("failed to convert response: %w", err)
	}
	respHeaders.Del("Content-Length")
	respHeaders.Set("Content-Type", "application/json")
	return status, geminiResp, respHeaders, nil, nil
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
	if apiType == config.APITypeResponses {
		openAIBody, err := convertResponsesToOpenAIChat(body)
		if err != nil {
			return nil, err
		}
		return convertOpenAIToGemini(openAIBody)
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
		} else if apiType == config.APITypeResponses {
			eventType := currentEventType
			if eventType == "" {
				if t, ok := event["type"].(string); ok {
					eventType = t
				}
			}
			switch eventType {
			case "response.output_text.delta":
				if delta, ok := event["delta"].(string); ok {
					textContent.WriteString(delta)
				}
			case "response.completed":
				if resp, ok := event["response"].(map[string]any); ok {
					if m, ok := resp["model"].(string); ok {
						model = m
					}
					if usage, ok := resp["usage"].(map[string]any); ok {
						if input, ok := usage["input_tokens"].(float64); ok {
							inputTokens = int(input)
						}
						if output, ok := usage["output_tokens"].(float64); ok {
							outputTokens = int(output)
						}
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
