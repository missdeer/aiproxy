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
		client:   newHTTPClient(timeout),
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

				status, respBody, respHeaders, streamResp, err := h.forwardRequest(upstream, mappedModel, body, req.Stream, r)
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
						log.Printf("[SUCCESS] Upstream %s streaming completed (%d bytes)", upstream.Name, result.BytesWritten)
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

				// Success - reset failure count
				h.balancer.RecordSuccess(upstream.Name, currentModel)
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

		// Update request body with the fallback model
		var bodyMap map[string]any
		if err := json.Unmarshal(body, &bodyMap); err == nil {
			bodyMap["model"] = currentModel
			if newBody, err := json.Marshal(bodyMap); err == nil {
				body = newBody
			}
		}
	}

	// All upstreams and fallbacks exhausted
	if !attemptedUpstream {
		h.writeOpenAIError(w, http.StatusBadRequest, "model_not_found", fmt.Sprintf("No upstream supports model: %s", originalModel))
		return
	}
	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	h.writeOpenAIError(w, lastStatus, "upstream_error", fmt.Sprintf("All upstreams failed: %v", lastErr))
}

func (h *OpenAIHandler) forwardRequest(upstream config.Upstream, model string, originalBody []byte, clientWantsStream bool, originalReq *http.Request) (int, []byte, http.Header, *http.Response, error) {
	var bodyMap map[string]any
	if err := json.Unmarshal(originalBody, &bodyMap); err != nil {
		return 0, nil, nil, nil, err
	}

	bodyMap["model"] = model

	apiType := upstream.GetAPIType()

	// Native OpenAI - direct passthrough without format conversion
	if apiType == config.APITypeOpenAI {
		modifiedBody, err := json.Marshal(bodyMap)
		if err != nil {
			return 0, nil, nil, nil, err
		}
		url := strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/chat/completions"
		if originalReq.URL.RawQuery != "" {
			url = url + "?" + originalReq.URL.RawQuery
		}

		// Streaming: return raw response for direct pipe to client
		if clientWantsStream {
			resp, err := doHTTPRequestStream(h.client, url, modifiedBody, upstream, config.APITypeOpenAI, originalReq, "")
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
		status, respBody, headers, err := doHTTPRequest(h.client, url, modifiedBody, upstream, config.APITypeOpenAI, originalReq, "")
		if err != nil {
			return 0, nil, nil, nil, err
		}
		return status, respBody, headers, nil, nil
	}

	// Convert OpenAI Chat request to canonical (Responses) format
	canonicalBody := convertOpenAIChatToResponsesRequest(bodyMap)
	canonicalBody["model"] = model
	if clientWantsStream {
		canonicalBody["stream"] = true
	}
	canonicalBytes, err := json.Marshal(canonicalBody)
	if err != nil {
		return 0, nil, nil, nil, err
	}

	// Send via OutboundSender
	sender := GetOutboundSender(apiType)
	status, respBody, respHeaders, respFormat, err := sender.Send(h.client, upstream, canonicalBytes, clientWantsStream, originalReq)
	if err != nil {
		return status, nil, nil, nil, err
	}

	// Error responses pass through without conversion
	if status >= 400 {
		return status, respBody, respHeaders, nil, nil
	}

	// Convert response back to OpenAI format
	if clientWantsStream {
		openAIResp, err := h.convertStreamToOpenAI(bytes.NewReader(respBody), respFormat, false)
		if err != nil {
			return 0, nil, nil, nil, fmt.Errorf("failed to convert stream response: %w", err)
		}
		respHeaders.Del("Content-Length")
		respHeaders.Del("Content-Encoding")
		respHeaders.Set("Content-Type", "text/event-stream")
		return status, openAIResp, respHeaders, nil, nil
	}

	// Special case: Responses format uses a different conversion function
	if respFormat == config.APITypeResponses {
		openAIResp, err := convertResponsesToOpenAIChat(respBody)
		if err != nil {
			return 0, nil, nil, nil, fmt.Errorf("failed to convert response: %w", err)
		}
		respHeaders.Del("Content-Length")
		respHeaders.Set("Content-Type", "application/json")
		return status, openAIResp, respHeaders, nil, nil
	}

	openAIResp, err := convertResponseToOpenAI(respBody, respFormat)
	if err != nil {
		return 0, nil, nil, nil, fmt.Errorf("failed to convert response: %w", err)
	}
	respHeaders.Del("Content-Length")
	respHeaders.Set("Content-Type", "application/json")
	return status, openAIResp, respHeaders, nil, nil
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
