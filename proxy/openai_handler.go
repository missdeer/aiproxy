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

// OpenAIHandler handles OpenAI-compatible API requests
type OpenAIHandler struct {
	cfg      *config.Config
	balancer *balancer.WeightedRoundRobin
	client   *http.Client
	mu       sync.RWMutex
}

// NewOpenAIHandler creates a new OpenAI-compatible handler
func NewOpenAIHandler(cfg *config.Config) *OpenAIHandler {
	timeout := time.Duration(cfg.UpstreamRequestTimeout) * time.Second
	return &OpenAIHandler{
		cfg:      cfg,
		balancer: balancer.NewWeightedRoundRobin(cfg.Upstreams),
		client:   &http.Client{Timeout: timeout},
	}
}

// UpdateConfig updates the handler's configuration
func (h *OpenAIHandler) UpdateConfig(cfg *config.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
	h.balancer.Update(cfg.Upstreams)
}

// OpenAI request/response types
type OpenAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []OpenAIMessage `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	Stop        any             `json:"stop,omitempty"`
}

type OpenAIMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or array of content parts
}

type OpenAIChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage,omitempty"`
}

type OpenAIChoice struct {
	Index        int            `json:"index"`
	Message      *OpenAIMessage `json:"message,omitempty"`
	Delta        *OpenAIMessage `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type OpenAIStreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
}

func (h *OpenAIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var req OpenAIChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[ERROR] Invalid JSON request: %v", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Extract prompt preview for logging
	promptPreview := extractOpenAIPromptPreview(req.Messages)
	log.Printf("[OPENAI REQUEST] Model: %s, Stream: %v, Prompt: %s", req.Model, req.Stream, promptPreview)

	originalModel := req.Model

	upstreams := h.balancer.GetAll()
	if len(upstreams) == 0 {
		log.Printf("[ERROR] No upstreams configured")
		http.Error(w, "No upstreams configured", http.StatusServiceUnavailable)
		return
	}

	// Filter upstreams that support the requested model and are available
	var supportedUpstreams []config.Upstream
	for _, u := range upstreams {
		if u.SupportsModel(originalModel) && h.balancer.IsAvailable(u.Name, originalModel) {
			supportedUpstreams = append(supportedUpstreams, u)
		}
	}

	if len(supportedUpstreams) == 0 {
		log.Printf("[ERROR] No available upstream supports model: %s", originalModel)
		h.writeOpenAIError(w, http.StatusBadRequest, "model_not_found", fmt.Sprintf("No upstream supports model: %s", originalModel))
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

		status, respBody, respHeaders, err := h.forwardRequest(upstream, mappedModel, body, req.Stream, r)
		if err != nil {
			log.Printf("[ERROR] Upstream %s connection error: %v", upstream.Name, err)
			if h.balancer.RecordFailure(upstream.Name, originalModel) {
				log.Printf("[CIRCUIT] Upstream %s model %s marked as unavailable after %d consecutive failures", upstream.Name, originalModel, 3)
			}
			lastErr = err
			lastStatus = http.StatusBadGateway
			continue
		}

		if status >= 400 {
			log.Printf("[ERROR] Upstream %s returned HTTP %d, response: %s", upstream.Name, status, truncateString(string(respBody), 200))
			if h.balancer.RecordFailure(upstream.Name, originalModel) {
				log.Printf("[CIRCUIT] Upstream %s model %s marked as unavailable after %d consecutive failures", upstream.Name, originalModel, 3)
			}
			lastErr = fmt.Errorf("upstream returned status %d", status)
			lastStatus = status
			continue
		}

		// Success - reset failure count
		h.balancer.RecordSuccess(upstream.Name, originalModel)
		log.Printf("[SUCCESS] Upstream %s returned HTTP %d", upstream.Name, status)

		// Copy response headers and body
		for k, vv := range respHeaders {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)

		if req.Stream {
			h.streamResponse(w, respBody)
		} else {
			w.Write(respBody)
		}
		return
	}

	// All upstreams failed
	log.Printf("[FAILED] All %d upstreams failed for model %s, last error: %v, last status: %d", len(ordered), originalModel, lastErr, lastStatus)
	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	h.writeOpenAIError(w, lastStatus, "upstream_error", fmt.Sprintf("All upstreams failed: %v", lastErr))
}

func (h *OpenAIHandler) forwardRequest(upstream config.Upstream, model string, originalBody []byte, clientWantsStream bool, originalReq *http.Request) (int, []byte, http.Header, error) {
	var bodyMap map[string]any
	if err := json.Unmarshal(originalBody, &bodyMap); err != nil {
		return 0, nil, nil, err
	}

	bodyMap["model"] = model

	// Check if we need to force streaming for this upstream
	forceStream := upstream.MustStream && !clientWantsStream
	if forceStream {
		bodyMap["stream"] = true
		log.Printf("[INFO] Forcing stream=true for upstream %s (mustStream enabled)", upstream.Name)
	}

	apiType := upstream.GetAPIType()
	var url string
	var modifiedBody []byte
	var err error

	// Convert OpenAI request to target API format
	switch apiType {
	case config.APITypeCodex:
		// Convert OpenAI chat to Responses format, then forward to Codex
		responsesBody := convertOpenAIChatToResponsesRequest(bodyMap)
		modifiedBody, err = json.Marshal(responsesBody)
		if err != nil {
			return 0, nil, nil, err
		}
		// ForwardToCodex returns Responses-format data (JSON if non-stream, SSE if stream).
		// Convert it to OpenAI format before returning.
		status, respBody, respHeaders, err := ForwardToCodex(h.client, upstream, modifiedBody, clientWantsStream)
		if err != nil {
			return status, nil, nil, err
		}
		if status >= 400 {
			return status, respBody, respHeaders, nil
		}
		if clientWantsStream {
			// Codex SSE uses Responses-format events; convert to OpenAI streaming chunks
			openAIResp, err := h.convertStreamToOpenAI(bytes.NewReader(respBody), config.APITypeResponses, false)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("failed to convert codex stream to openai: %w", err)
			}
			respHeaders.Set("Content-Type", "text/event-stream")
			return status, openAIResp, respHeaders, nil
		}
		// Non-stream: ForwardToCodex already assembled JSON in Responses format
		openAIResp, err := convertResponsesToOpenAIChat(respBody)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("failed to convert codex response to openai: %w", err)
		}
		respHeaders.Set("Content-Type", "application/json")
		return status, openAIResp, respHeaders, nil

	case config.APITypeOpenAI:
		// Native OpenAI - no conversion needed
		modifiedBody, err = json.Marshal(bodyMap)
		if err != nil {
			return 0, nil, nil, err
		}
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/chat/completions"
		// Pass through query parameters when API types match
		if originalReq.URL.RawQuery != "" {
			url = url + "?" + originalReq.URL.RawQuery
		}

	case config.APITypeResponses:
		// Convert OpenAI chat to Responses format
		responsesBody := convertOpenAIChatToResponsesRequest(bodyMap)
		modifiedBody, err = json.Marshal(responsesBody)
		if err != nil {
			return 0, nil, nil, err
		}
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/responses"

	case config.APITypeAnthropic:
		// Convert OpenAI to Anthropic format
		anthropicBody := convertOpenAIChatToAnthropicRequest(bodyMap)
		modifiedBody, err = json.Marshal(anthropicBody)
		if err != nil {
			return 0, nil, nil, err
		}
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/messages"

	case config.APITypeGemini:
		// Convert OpenAI to Gemini format
		geminiBody := convertOpenAIChatToGeminiRequest(bodyMap)
		modifiedBody, err = json.Marshal(geminiBody)
		if err != nil {
			return 0, nil, nil, err
		}
		action := "generateContent"
		if clientWantsStream || forceStream {
			action = "streamGenerateContent"
		}
		url = fmt.Sprintf("%s/v1beta/models/%s:%s", strings.TrimSuffix(upstream.BaseURL, "/"), model, action)

	default:
		modifiedBody, err = json.Marshal(bodyMap)
		if err != nil {
			return 0, nil, nil, err
		}
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/chat/completions"
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(modifiedBody))
	if err != nil {
		return 0, nil, nil, err
	}

	// Copy headers from original request
	for k, vv := range originalReq.Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	stripHopByHopHeaders(req.Header)

	// Set authentication based on API type
	switch apiType {
	case config.APITypeAnthropic:
		req.Header.Set("x-api-key", upstream.Token)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Del("Authorization")
	case config.APITypeOpenAI, config.APITypeResponses:
		req.Header.Set("Authorization", "Bearer "+upstream.Token)
		req.Header.Del("x-api-key")
	case config.APITypeGemini:
		if !strings.Contains(url, "key=") {
			url = url + "?key=" + upstream.Token
			req.URL, _ = req.URL.Parse(url)
		}
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Del("Content-Length")

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	// Handle response conversion back to OpenAI format
	// APITypeOpenAI doesn't need conversion
	// APITypeResponses needs conversion from Responses API format to OpenAI chat format
	if resp.StatusCode < 400 && apiType != config.APITypeOpenAI {
		if apiType == config.APITypeResponses {
			// Convert Responses API response to OpenAI chat format
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return 0, nil, nil, err
			}
			openAIResp, err := convertResponsesToOpenAIChat(respBody)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("failed to convert responses to openai: %w", err)
			}
			headers := resp.Header.Clone()
			stripHopByHopHeaders(headers)
			headers.Del("Content-Length")
			headers.Set("Content-Type", "application/json")
			return resp.StatusCode, openAIResp, headers, nil
		}
		if clientWantsStream || forceStream {
			// Convert streaming response to OpenAI format
			openAIResp, err := h.convertStreamToOpenAI(resp.Body, apiType, !clientWantsStream)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("failed to convert stream response: %w", err)
			}
			headers := resp.Header.Clone()
			stripHopByHopHeaders(headers)
			headers.Del("Content-Length")
			headers.Del("Content-Encoding")
			if clientWantsStream {
				headers.Set("Content-Type", "text/event-stream")
			} else {
				headers.Set("Content-Type", "application/json")
			}
			return resp.StatusCode, openAIResp, headers, nil
		} else {
			// Convert non-streaming response to OpenAI format
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return 0, nil, nil, err
			}
			openAIResp, err := convertResponseToOpenAI(respBody, apiType)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("failed to convert response: %w", err)
			}
			headers := resp.Header.Clone()
			stripHopByHopHeaders(headers)
			headers.Del("Content-Length")
			headers.Set("Content-Type", "application/json")
			return resp.StatusCode, openAIResp, headers, nil
		}
	}

	// For native OpenAI upstream or error responses, pass through
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}

	return resp.StatusCode, respBody, resp.Header, nil
}

// convertStreamToOpenAI converts Anthropic or Gemini streaming response to OpenAI format
func (h *OpenAIHandler) convertStreamToOpenAI(reader io.Reader, apiType config.APIType, toNonStream bool) ([]byte, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var (
		messageID         string
		model             string
		inputTokens       int
		outputTokens      int
		textContent       strings.Builder
		currentEventType  string
		dataBuilder       strings.Builder
		chunks            []byte
		stopReason        string
		sentRoleChunk     bool
		sentTerminalChunk bool
	)

	messageID = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

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

		switch apiType {
		case config.APITypeAnthropic:
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
				if !toNonStream {
					chunk := OpenAIStreamChunk{
						ID:      messageID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   model,
						Choices: []OpenAIChoice{
							{
								Index:        0,
								Delta:        &OpenAIMessage{Role: "assistant"},
								FinishReason: nil,
							},
						},
					}
					chunkBytes, _ := json.Marshal(chunk)
					chunks = append(chunks, []byte("data: ")...)
					chunks = append(chunks, chunkBytes...)
					chunks = append(chunks, []byte("\n\n")...)
					sentRoleChunk = true
				}

			case "content_block_delta":
				if delta, ok := event["delta"].(map[string]any); ok {
					if deltaType, ok := delta["type"].(string); ok && deltaType == "text_delta" {
						if text, ok := delta["text"].(string); ok {
							textContent.WriteString(text)
							if !toNonStream {
								chunk := OpenAIStreamChunk{
									ID:      messageID,
									Object:  "chat.completion.chunk",
									Created: time.Now().Unix(),
									Model:   model,
									Choices: []OpenAIChoice{
										{
											Index:        0,
											Delta:        &OpenAIMessage{Content: text},
											FinishReason: nil,
										},
									},
								}
								chunkBytes, _ := json.Marshal(chunk)
								chunks = append(chunks, []byte("data: ")...)
								chunks = append(chunks, chunkBytes...)
								chunks = append(chunks, []byte("\n\n")...)
							}
						}
					}
				}

			case "message_delta":
				if delta, ok := event["delta"].(map[string]any); ok {
					if sr, ok := delta["stop_reason"].(string); ok {
						stopReason = sr
					}
				}
				if usage, ok := event["usage"].(map[string]any); ok {
					if output, ok := usage["output_tokens"].(float64); ok {
						outputTokens = int(output)
					}
				}

			case "message_stop":
				if !toNonStream {
					reason := mapAnthropicStopReason(stopReason)
					chunk := OpenAIStreamChunk{
						ID:      messageID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   model,
						Choices: []OpenAIChoice{
							{
								Index:        0,
								Delta:        &OpenAIMessage{},
								FinishReason: &reason,
							},
						},
					}
					chunkBytes, _ := json.Marshal(chunk)
					chunks = append(chunks, []byte("data: ")...)
					chunks = append(chunks, chunkBytes...)
					chunks = append(chunks, []byte("\n\n")...)
					chunks = append(chunks, []byte("data: [DONE]\n\n")...)
					sentTerminalChunk = true
				}
			}

		case config.APITypeGemini:
			if candidates, ok := event["candidates"].([]any); ok && len(candidates) > 0 {
				if candidate, ok := candidates[0].(map[string]any); ok {
					if content, ok := candidate["content"].(map[string]any); ok {
						if parts, ok := content["parts"].([]any); ok {
							for _, part := range parts {
								if partMap, ok := part.(map[string]any); ok {
									if text, ok := partMap["text"].(string); ok {
										textContent.WriteString(text)
										if !toNonStream {
											chunk := OpenAIStreamChunk{
												ID:      messageID,
												Object:  "chat.completion.chunk",
												Created: time.Now().Unix(),
												Model:   model,
												Choices: []OpenAIChoice{
													{
														Index:        0,
														Delta:        &OpenAIMessage{Content: text},
														FinishReason: nil,
													},
												},
											}
											chunkBytes, _ := json.Marshal(chunk)
											chunks = append(chunks, []byte("data: ")...)
											chunks = append(chunks, chunkBytes...)
											chunks = append(chunks, []byte("\n\n")...)
										}
									}
								}
							}
						}
					}
					if fr, ok := candidate["finishReason"].(string); ok && fr != "" {
						stopReason = fr
					}
				}
			}
			if usage, ok := event["usageMetadata"].(map[string]any); ok {
				if prompt, ok := usage["promptTokenCount"].(float64); ok {
					inputTokens = int(prompt)
				}
				if candidates, ok := usage["candidatesTokenCount"].(float64); ok {
					outputTokens = int(candidates)
				}
			}
			if mv, ok := event["modelVersion"].(string); ok {
				model = mv
			}

		case config.APITypeResponses:
			eventType := currentEventType
			if eventType == "" {
				if t, ok := event["type"].(string); ok {
					eventType = t
				}
			}

			switch eventType {
			case "response.output_text.delta":
				if text, ok := event["delta"].(string); ok && text != "" {
					textContent.WriteString(text)
					if !toNonStream {
						if !sentRoleChunk {
							chunk := OpenAIStreamChunk{
								ID:      messageID,
								Object:  "chat.completion.chunk",
								Created: time.Now().Unix(),
								Model:   model,
								Choices: []OpenAIChoice{
									{
										Index:        0,
										Delta:        &OpenAIMessage{Role: "assistant"},
										FinishReason: nil,
									},
								},
							}
							chunkBytes, _ := json.Marshal(chunk)
							chunks = append(chunks, []byte("data: ")...)
							chunks = append(chunks, chunkBytes...)
							chunks = append(chunks, []byte("\n\n")...)
							sentRoleChunk = true
						}

						chunk := OpenAIStreamChunk{
							ID:      messageID,
							Object:  "chat.completion.chunk",
							Created: time.Now().Unix(),
							Model:   model,
							Choices: []OpenAIChoice{
								{
									Index:        0,
									Delta:        &OpenAIMessage{Content: text},
									FinishReason: nil,
								},
							},
						}
						chunkBytes, _ := json.Marshal(chunk)
						chunks = append(chunks, []byte("data: ")...)
						chunks = append(chunks, chunkBytes...)
						chunks = append(chunks, []byte("\n\n")...)
					}
				}
			case "response.completed":
				stopReason = "stop"
				if responseObj, ok := event["response"].(map[string]any); ok {
					if m, ok := responseObj["model"].(string); ok {
						model = m
					}
					if usage, ok := responseObj["usage"].(map[string]any); ok {
						if in, ok := usage["input_tokens"].(float64); ok {
							inputTokens = int(in)
						}
						if out, ok := usage["output_tokens"].(float64); ok {
							outputTokens = int(out)
						}
					}
				}

				if !toNonStream && !sentTerminalChunk {
					reason := "stop"
					chunk := OpenAIStreamChunk{
						ID:      messageID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   model,
						Choices: []OpenAIChoice{
							{
								Index:        0,
								Delta:        &OpenAIMessage{},
								FinishReason: &reason,
							},
						},
					}
					chunkBytes, _ := json.Marshal(chunk)
					chunks = append(chunks, []byte("data: ")...)
					chunks = append(chunks, chunkBytes...)
					chunks = append(chunks, []byte("\n\n")...)
					chunks = append(chunks, []byte("data: [DONE]\n\n")...)
					sentTerminalChunk = true
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

	if toNonStream {
		// Return complete OpenAI response
		var finishReason *string
		if stopReason != "" {
			if apiType == config.APITypeAnthropic {
				reason := mapAnthropicStopReason(stopReason)
				finishReason = &reason
			} else if apiType == config.APITypeResponses {
				reason := "stop"
				finishReason = &reason
			} else {
				reason := mapGeminiToOpenAIFinishReason(stopReason)
				finishReason = &reason
			}
		}

		openAIResp := OpenAIChatResponse{
			ID:      messageID,
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []OpenAIChoice{
				{
					Index: 0,
					Message: &OpenAIMessage{
						Role:    "assistant",
						Content: textContent.String(),
					},
					FinishReason: finishReason,
				},
			},
			Usage: OpenAIUsage{
				PromptTokens:     inputTokens,
				CompletionTokens: outputTokens,
				TotalTokens:      inputTokens + outputTokens,
			},
		}
		return json.Marshal(openAIResp)
	}

	// Send final done marker if not already sent
	if !sentTerminalChunk && (apiType == config.APITypeGemini || apiType == config.APITypeResponses) && len(chunks) > 0 {
		reason := "stop"
		if apiType == config.APITypeGemini {
			reason = mapGeminiToOpenAIFinishReason(stopReason)
		}
		chunk := OpenAIStreamChunk{
			ID:      messageID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []OpenAIChoice{
				{
					Index:        0,
					Delta:        &OpenAIMessage{},
					FinishReason: &reason,
				},
			},
		}
		chunkBytes, _ := json.Marshal(chunk)
		chunks = append(chunks, []byte("data: ")...)
		chunks = append(chunks, chunkBytes...)
		chunks = append(chunks, []byte("\n\n")...)
		chunks = append(chunks, []byte("data: [DONE]\n\n")...)
	}

	return chunks, nil
}

func (h *OpenAIHandler) streamResponse(w http.ResponseWriter, body []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Write(body)
		return
	}

	w.Write(body)
	flusher.Flush()
}

func (h *OpenAIHandler) writeOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    errType,
		},
	})
}

func extractOpenAIPromptPreview(messages []OpenAIMessage) string {
	if len(messages) == 0 {
		return "(empty)"
	}

	lastMsg := messages[len(messages)-1]
	switch c := lastMsg.Content.(type) {
	case string:
		return truncateString(c, 100)
	case []any:
		for _, part := range c {
			if partMap, ok := part.(map[string]any); ok {
				if text, ok := partMap["text"].(string); ok {
					return truncateString(text, 100)
				}
			}
		}
	}

	return "(unknown format)"
}

// convertAnthropicToOpenAIResponse converts Anthropic response to OpenAI format
func convertAnthropicToOpenAIResponse(anthropicBody []byte) ([]byte, error) {
	var anthropicResp struct {
		ID           string `json:"id"`
		Type         string `json:"type"`
		Role         string `json:"role"`
		Content      []any  `json:"content"`
		Model        string `json:"model"`
		StopReason   string `json:"stop_reason"`
		StopSequence string `json:"stop_sequence"`
		Usage        struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(anthropicBody, &anthropicResp); err != nil {
		return nil, err
	}

	// Extract text content
	var textContent string
	for _, block := range anthropicResp.Content {
		if blockMap, ok := block.(map[string]any); ok {
			if blockType, ok := blockMap["type"].(string); ok && blockType == "text" {
				if text, ok := blockMap["text"].(string); ok {
					textContent += text
				}
			}
		}
	}

	// Map stop reason
	var finishReason *string
	if anthropicResp.StopReason != "" {
		reason := mapAnthropicStopReason(anthropicResp.StopReason)
		finishReason = &reason
	}

	openAIResp := OpenAIChatResponse{
		ID:      "chatcmpl-" + anthropicResp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   anthropicResp.Model,
		Choices: []OpenAIChoice{
			{
				Index: 0,
				Message: &OpenAIMessage{
					Role:    "assistant",
					Content: textContent,
				},
				FinishReason: finishReason,
			},
		},
		Usage: OpenAIUsage{
			PromptTokens:     anthropicResp.Usage.InputTokens,
			CompletionTokens: anthropicResp.Usage.OutputTokens,
			TotalTokens:      anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
		},
	}

	return json.Marshal(openAIResp)
}

func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}
