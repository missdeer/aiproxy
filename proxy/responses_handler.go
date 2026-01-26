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
	"time"

	"github.com/missdeer/aiproxy/balancer"
	"github.com/missdeer/aiproxy/config"
)

// ResponsesHandler handles OpenAI Responses API requests (/v1/responses)
type ResponsesHandler struct {
	cfg      *config.Config
	balancer *balancer.WeightedRoundRobin
	client   *http.Client
}

// NewResponsesHandler creates a new Responses API handler
func NewResponsesHandler(cfg *config.Config) *ResponsesHandler {
	return &ResponsesHandler{
		cfg:      cfg,
		balancer: balancer.NewWeightedRoundRobin(cfg.Upstreams),
		client:   &http.Client{},
	}
}

// ResponsesRequest represents OpenAI Responses API request
type ResponsesRequest struct {
	Model              string  `json:"model"`
	Input              any     `json:"input"` // string or array of input items
	Instructions       string  `json:"instructions,omitempty"`
	Stream             bool    `json:"stream,omitempty"`
	MaxOutputTokens    int     `json:"max_output_tokens,omitempty"`
	Temperature        float64 `json:"temperature,omitempty"`
	TopP               float64 `json:"top_p,omitempty"`
	PreviousResponseID string  `json:"previous_response_id,omitempty"`
}

// ResponsesResponse represents OpenAI Responses API response
type ResponsesResponse struct {
	ID        string           `json:"id"`
	Object    string           `json:"object"`
	CreatedAt int64            `json:"created_at"`
	Model     string           `json:"model"`
	Output    []ResponseOutput `json:"output"`
	Usage     ResponsesUsage   `json:"usage,omitempty"`
	Status    string           `json:"status,omitempty"`
	Error     *ResponsesError  `json:"error,omitempty"`
}

type ResponseOutput struct {
	Type    string            `json:"type"`
	ID      string            `json:"id,omitempty"`
	Role    string            `json:"role,omitempty"`
	Content []ResponseContent `json:"content,omitempty"`
	Status  string            `json:"status,omitempty"`
}

type ResponseContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type ResponsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Streaming event types
type ResponsesStreamEvent struct {
	Type     string             `json:"type"`
	Response *ResponsesResponse `json:"response,omitempty"`
	Item     *ResponseOutput    `json:"item,omitempty"`
	Delta    *ResponsesDelta    `json:"delta,omitempty"`
	Usage    *ResponsesUsage    `json:"usage,omitempty"`
}

type ResponsesDelta struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

func (h *ResponsesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[ERROR] Invalid JSON request: %v", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Extract prompt preview for logging
	promptPreview := extractResponsesPromptPreview(req.Input)
	log.Printf("[RESPONSES REQUEST] Model: %s, Stream: %v, Prompt: %s", req.Model, req.Stream, promptPreview)

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
		if u.SupportsModel(originalModel) && h.balancer.IsAvailable(u.Name) {
			supportedUpstreams = append(supportedUpstreams, u)
		}
	}

	if len(supportedUpstreams) == 0 {
		log.Printf("[ERROR] No available upstream supports model: %s", originalModel)
		h.writeResponsesError(w, http.StatusBadRequest, "model_not_found", fmt.Sprintf("No upstream supports model: %s", originalModel))
		return
	}

	log.Printf("[INFO] Found %d available upstreams for model %s", len(supportedUpstreams), originalModel)

	// Reorder upstreams: start from the one returned by Next()
	next := h.balancer.Next()
	startIdx := 0
	for i, u := range supportedUpstreams {
		if u.Name == next.Name {
			startIdx = i
			break
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

		// Success - reset failure count
		h.balancer.RecordSuccess(upstream.Name)
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
	h.writeResponsesError(w, lastStatus, "upstream_error", fmt.Sprintf("All upstreams failed: %v", lastErr))
}

func (h *ResponsesHandler) forwardRequest(upstream config.Upstream, model string, originalBody []byte, clientWantsStream bool, originalReq *http.Request) (int, []byte, http.Header, error) {
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

	// Convert request format based on API type
	var url string
	switch apiType {
	case config.APITypeAnthropic:
		bodyMap = convertResponsesToAnthropicRequest(bodyMap)
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/messages"
	case config.APITypeGemini:
		bodyMap = convertResponsesToGeminiRequest(bodyMap)
		action := "generateContent"
		if clientWantsStream || forceStream {
			action = "streamGenerateContent"
		}
		url = fmt.Sprintf("%s/v1beta/models/%s:%s",
			strings.TrimSuffix(upstream.BaseURL, "/"), model, action)
	case config.APITypeOpenAI:
		// Convert to chat/completions format
		bodyMap = convertResponsesToChatRequest(bodyMap)
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/chat/completions"
	case config.APITypeResponses:
		// Native Responses API - no conversion needed
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/responses"
	default:
		bodyMap = convertResponsesToAnthropicRequest(bodyMap)
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/messages"
	}

	modifiedBody, err := json.Marshal(bodyMap)
	if err != nil {
		return 0, nil, nil, err
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
	case config.APITypeGemini:
		if !strings.Contains(url, "key=") {
			url = url + "?key=" + upstream.Token
			req.URL, _ = req.URL.Parse(url)
		}
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	case config.APITypeOpenAI, config.APITypeResponses:
		req.Header.Set("Authorization", "Bearer "+upstream.Token)
		req.Header.Del("x-api-key")
	default:
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
	if resp.StatusCode < 400 {
		// APITypeResponses doesn't need conversion - it's native Responses API
		needsConversion := apiType != config.APITypeResponses

		if needsConversion {
			if forceStream || (bodyMap["stream"] == true) {
				// Handle streaming response
				responsesResp, err := h.convertStreamToResponses(resp.Body, clientWantsStream, apiType)
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
				return resp.StatusCode, responsesResp, headers, nil
			} else {
				// Handle non-streaming response
				respBody, err := io.ReadAll(resp.Body)
				if err != nil {
					return 0, nil, nil, err
				}
				responsesResp, err := convertToResponsesFormat(respBody, apiType)
				if err != nil {
					return 0, nil, nil, fmt.Errorf("failed to convert response: %w", err)
				}
				headers := resp.Header.Clone()
				stripHopByHopHeaders(headers)
				headers.Del("Content-Length")
				headers.Set("Content-Type", "application/json")
				return resp.StatusCode, responsesResp, headers, nil
			}
		}
	}

	// For native Responses API upstream or error responses, pass through
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}

	return resp.StatusCode, respBody, resp.Header, nil
}

func (h *ResponsesHandler) streamResponse(w http.ResponseWriter, body []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Write(body)
		return
	}

	w.Write(body)
	flusher.Flush()
}

func (h *ResponsesHandler) writeResponsesError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"code":    code,
		},
	})
}

func extractResponsesPromptPreview(input any) string {
	switch v := input.(type) {
	case string:
		return truncateString(v, 100)
	case []any:
		// Look for user message content
		for _, item := range v {
			if itemMap, ok := item.(map[string]any); ok {
				if role, _ := itemMap["role"].(string); role == "user" {
					if content, ok := itemMap["content"].(string); ok {
						return truncateString(content, 100)
					}
					if contentArr, ok := itemMap["content"].([]any); ok {
						for _, c := range contentArr {
							if cMap, ok := c.(map[string]any); ok {
								if text, ok := cMap["text"].(string); ok {
									return truncateString(text, 100)
								}
							}
						}
					}
				}
			}
		}
	}
	return "(unknown format)"
}

// convertResponsesToAnthropicRequest converts Responses API request to Anthropic format
func convertResponsesToAnthropicRequest(req map[string]any, defaultMaxTokens ...int) map[string]any {
	anthropicReq := make(map[string]any)

	// Copy model
	if model, ok := req["model"]; ok {
		anthropicReq["model"] = model
	}

	// Convert max_output_tokens to max_tokens
	if maxTokens, ok := req["max_output_tokens"]; ok {
		anthropicReq["max_tokens"] = maxTokens
	} else {
		maxTokensDefault := 4096
		if len(defaultMaxTokens) > 0 && defaultMaxTokens[0] > 0 {
			maxTokensDefault = defaultMaxTokens[0]
		}
		anthropicReq["max_tokens"] = maxTokensDefault
	}

	// Copy optional parameters
	if temp, ok := req["temperature"]; ok {
		anthropicReq["temperature"] = temp
	}
	if topP, ok := req["top_p"]; ok {
		anthropicReq["top_p"] = topP
	}
	if stream, ok := req["stream"]; ok {
		anthropicReq["stream"] = stream
	}

	// Set system from instructions
	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		anthropicReq["system"] = instructions
	}

	// Convert input to messages
	messages := convertInputToAnthropicMessages(req["input"])
	anthropicReq["messages"] = messages

	return anthropicReq
}

// convertResponsesToChatRequest converts Responses API request to Chat Completions format
func convertResponsesToChatRequest(req map[string]any) map[string]any {
	chatReq := make(map[string]any)

	// Copy model
	if model, ok := req["model"]; ok {
		chatReq["model"] = model
	}

	// Convert max_output_tokens to max_tokens
	if maxTokens, ok := req["max_output_tokens"]; ok {
		chatReq["max_tokens"] = maxTokens
	}

	// Copy optional parameters
	if temp, ok := req["temperature"]; ok {
		chatReq["temperature"] = temp
	}
	if topP, ok := req["top_p"]; ok {
		chatReq["top_p"] = topP
	}
	if stream, ok := req["stream"]; ok {
		chatReq["stream"] = stream
	}

	// Convert input to messages
	var messages []map[string]any

	// Add system message from instructions
	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instructions,
		})
	}

	// Convert input
	messages = append(messages, convertInputToChatMessages(req["input"])...)
	chatReq["messages"] = messages

	return chatReq
}

func convertInputToAnthropicMessages(input any) []map[string]any {
	var messages []map[string]any

	switch v := input.(type) {
	case string:
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": v,
		})
	case []any:
		for _, item := range v {
			if itemMap, ok := item.(map[string]any); ok {
				role, _ := itemMap["role"].(string)
				content := itemMap["content"]

				if role == "system" {
					continue // System is handled separately in Anthropic
				}

				msg := map[string]any{
					"role":    role,
					"content": content,
				}
				messages = append(messages, msg)
			}
		}
	}

	return messages
}

func convertInputToChatMessages(input any) []map[string]any {
	var messages []map[string]any

	switch v := input.(type) {
	case string:
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": v,
		})
	case []any:
		for _, item := range v {
			if itemMap, ok := item.(map[string]any); ok {
				role, _ := itemMap["role"].(string)
				content := itemMap["content"]

				msg := map[string]any{
					"role":    role,
					"content": content,
				}
				messages = append(messages, msg)
			}
		}
	}

	return messages
}

// convertToResponsesFormat converts Anthropic, Gemini, or Chat Completions response to Responses API format
func convertToResponsesFormat(body []byte, apiType config.APIType) ([]byte, error) {
	switch apiType {
	case config.APITypeAnthropic:
		return convertAnthropicToResponses(body)
	case config.APITypeGemini:
		return convertGeminiToResponses(body)
	default:
		return convertChatToResponses(body)
	}
}

func convertAnthropicToResponses(body []byte) ([]byte, error) {
	var anthropicResp struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Content    []any  `json:"content"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &anthropicResp); err != nil {
		return nil, err
	}

	// Extract text content
	var contents []ResponseContent
	for _, block := range anthropicResp.Content {
		if blockMap, ok := block.(map[string]any); ok {
			if blockType, ok := blockMap["type"].(string); ok && blockType == "text" {
				if text, ok := blockMap["text"].(string); ok {
					contents = append(contents, ResponseContent{
						Type: "output_text",
						Text: text,
					})
				}
			}
		}
	}

	resp := ResponsesResponse{
		ID:        "resp_" + anthropicResp.ID,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     anthropicResp.Model,
		Status:    "completed",
		Output: []ResponseOutput{
			{
				Type:    "message",
				ID:      "msg_" + anthropicResp.ID,
				Role:    "assistant",
				Content: contents,
				Status:  "completed",
			},
		},
		Usage: ResponsesUsage{
			InputTokens:  anthropicResp.Usage.InputTokens,
			OutputTokens: anthropicResp.Usage.OutputTokens,
			TotalTokens:  anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
		},
	}

	return json.Marshal(resp)
}

func convertChatToResponses(body []byte) ([]byte, error) {
	var chatResp OpenAIChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, err
	}

	var contents []ResponseContent
	if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message != nil {
		if text, ok := chatResp.Choices[0].Message.Content.(string); ok {
			contents = append(contents, ResponseContent{
				Type: "output_text",
				Text: text,
			})
		}
	}

	resp := ResponsesResponse{
		ID:        "resp_" + chatResp.ID,
		Object:    "response",
		CreatedAt: chatResp.Created,
		Model:     chatResp.Model,
		Status:    "completed",
		Output: []ResponseOutput{
			{
				Type:    "message",
				ID:      "msg_" + chatResp.ID,
				Role:    "assistant",
				Content: contents,
				Status:  "completed",
			},
		},
		Usage: ResponsesUsage{
			InputTokens:  chatResp.Usage.PromptTokens,
			OutputTokens: chatResp.Usage.CompletionTokens,
			TotalTokens:  chatResp.Usage.TotalTokens,
		},
	}

	return json.Marshal(resp)
}

func convertGeminiToResponses(body []byte) ([]byte, error) {
	var geminiResp GeminiResponse
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return nil, err
	}

	var contents []ResponseContent
	if len(geminiResp.Candidates) > 0 {
		for _, part := range geminiResp.Candidates[0].Content.Parts {
			if part.Text != "" {
				contents = append(contents, ResponseContent{
					Type: "output_text",
					Text: part.Text,
				})
			}
		}
	}

	var inputTokens, outputTokens int
	if geminiResp.UsageMetadata != nil {
		inputTokens = geminiResp.UsageMetadata.PromptTokenCount
		outputTokens = geminiResp.UsageMetadata.CandidatesTokenCount
	}

	responseID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	resp := ResponsesResponse{
		ID:        responseID,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     geminiResp.ModelVersion,
		Status:    "completed",
		Output: []ResponseOutput{
			{
				Type:    "message",
				ID:      fmt.Sprintf("msg_%d", time.Now().UnixNano()),
				Role:    "assistant",
				Content: contents,
				Status:  "completed",
			},
		},
		Usage: ResponsesUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		},
	}

	return json.Marshal(resp)
}

// convertStreamToResponses converts streaming response to Responses API format
func (h *ResponsesHandler) convertStreamToResponses(reader io.Reader, clientWantsStream bool, apiType config.APIType) ([]byte, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var (
		messageID        string
		model            string
		inputTokens      int
		outputTokens     int
		currentEventType string
		dataBuilder      strings.Builder
		textContent      strings.Builder
		chunks           []byte
		responseID       string
	)

	responseID = fmt.Sprintf("resp_%d", time.Now().UnixNano())

	flushEvent := func() error {
		if dataBuilder.Len() == 0 {
			return nil
		}
		data := strings.TrimSpace(dataBuilder.String())
		dataBuilder.Reset()

		if data == "" || data == "[DONE]" {
			return nil
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil
		}

		if apiType == config.APITypeAnthropic {
			return h.processAnthropicStreamEvent(event, currentEventType, &messageID, &model, &inputTokens, &outputTokens, &textContent, &chunks, responseID, clientWantsStream)
		}
		if apiType == config.APITypeGemini {
			return h.processGeminiStreamEvent(event, &messageID, &model, &inputTokens, &outputTokens, &textContent, &chunks, responseID, clientWantsStream)
		}
		return h.processOpenAIStreamEvent(event, &messageID, &model, &outputTokens, &textContent, &chunks, responseID, clientWantsStream)
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

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	flushEvent()

	if clientWantsStream {
		return chunks, nil
	}

	// Return non-streaming response
	var contents []ResponseContent
	if textContent.Len() > 0 {
		contents = append(contents, ResponseContent{
			Type: "output_text",
			Text: textContent.String(),
		})
	}

	resp := ResponsesResponse{
		ID:        responseID,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     model,
		Status:    "completed",
		Output: []ResponseOutput{
			{
				Type:    "message",
				ID:      "msg_" + messageID,
				Role:    "assistant",
				Content: contents,
				Status:  "completed",
			},
		},
		Usage: ResponsesUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		},
	}

	return json.Marshal(resp)
}

func (h *ResponsesHandler) processAnthropicStreamEvent(event map[string]any, eventType string, messageID, model *string, inputTokens, outputTokens *int, textContent *strings.Builder, chunks *[]byte, responseID string, clientWantsStream bool) error {
	if eventType == "" {
		if t, ok := event["type"].(string); ok {
			eventType = t
		}
	}

	switch eventType {
	case "message_start":
		if msg, ok := event["message"].(map[string]any); ok {
			if id, ok := msg["id"].(string); ok {
				*messageID = id
			}
			if m, ok := msg["model"].(string); ok {
				*model = m
			}
			if usage, ok := msg["usage"].(map[string]any); ok {
				if input, ok := usage["input_tokens"].(float64); ok {
					*inputTokens = int(input)
				}
			}
		}
		if clientWantsStream {
			// Send response.created event
			evt := map[string]any{
				"type": "response.created",
				"response": map[string]any{
					"id":         responseID,
					"object":     "response",
					"created_at": time.Now().Unix(),
					"model":      *model,
					"status":     "in_progress",
				},
			}
			evtBytes, _ := json.Marshal(evt)
			*chunks = append(*chunks, []byte("event: response.created\ndata: ")...)
			*chunks = append(*chunks, evtBytes...)
			*chunks = append(*chunks, []byte("\n\n")...)
		}

	case "content_block_delta":
		if delta, ok := event["delta"].(map[string]any); ok {
			if deltaType, ok := delta["type"].(string); ok && deltaType == "text_delta" {
				if text, ok := delta["text"].(string); ok {
					textContent.WriteString(text)
					if clientWantsStream {
						evt := map[string]any{
							"type": "response.output_text.delta",
							"delta": map[string]any{
								"type": "output_text_delta",
								"text": text,
							},
						}
						evtBytes, _ := json.Marshal(evt)
						*chunks = append(*chunks, []byte("event: response.output_text.delta\ndata: ")...)
						*chunks = append(*chunks, evtBytes...)
						*chunks = append(*chunks, []byte("\n\n")...)
					}
				}
			}
		}

	case "message_delta":
		if usage, ok := event["usage"].(map[string]any); ok {
			if output, ok := usage["output_tokens"].(float64); ok {
				*outputTokens = int(output)
			}
		}

	case "message_stop":
		if clientWantsStream {
			// Send response.completed event
			evt := map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":         responseID,
					"object":     "response",
					"created_at": time.Now().Unix(),
					"model":      *model,
					"status":     "completed",
				},
			}
			evtBytes, _ := json.Marshal(evt)
			*chunks = append(*chunks, []byte("event: response.completed\ndata: ")...)
			*chunks = append(*chunks, evtBytes...)
			*chunks = append(*chunks, []byte("\n\n")...)
		}
	}

	return nil
}

func (h *ResponsesHandler) processOpenAIStreamEvent(event map[string]any, messageID, model *string, outputTokens *int, textContent *strings.Builder, chunks *[]byte, responseID string, clientWantsStream bool) error {
	if id, ok := event["id"].(string); ok {
		*messageID = id
	}
	if m, ok := event["model"].(string); ok {
		*model = m
	}

	if choices, ok := event["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if delta, ok := choice["delta"].(map[string]any); ok {
				if content, ok := delta["content"].(string); ok {
					textContent.WriteString(content)
					if clientWantsStream {
						evt := map[string]any{
							"type": "response.output_text.delta",
							"delta": map[string]any{
								"type": "output_text_delta",
								"text": content,
							},
						}
						evtBytes, _ := json.Marshal(evt)
						*chunks = append(*chunks, []byte("event: response.output_text.delta\ndata: ")...)
						*chunks = append(*chunks, evtBytes...)
						*chunks = append(*chunks, []byte("\n\n")...)
					}
				}
			}
			if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
				if clientWantsStream {
					evt := map[string]any{
						"type": "response.completed",
						"response": map[string]any{
							"id":         responseID,
							"object":     "response",
							"created_at": time.Now().Unix(),
							"model":      *model,
							"status":     "completed",
						},
					}
					evtBytes, _ := json.Marshal(evt)
					*chunks = append(*chunks, []byte("event: response.completed\ndata: ")...)
					*chunks = append(*chunks, evtBytes...)
					*chunks = append(*chunks, []byte("\n\n")...)
				}
			}
		}
	}

	return nil
}

func (h *ResponsesHandler) processGeminiStreamEvent(event map[string]any, messageID, model *string, inputTokens, outputTokens *int, textContent *strings.Builder, chunks *[]byte, responseID string, clientWantsStream bool) error {
	// Extract model version
	if mv, ok := event["modelVersion"].(string); ok {
		*model = mv
	}

	// Extract usage metadata
	if usage, ok := event["usageMetadata"].(map[string]any); ok {
		if prompt, ok := usage["promptTokenCount"].(float64); ok {
			*inputTokens = int(prompt)
		}
		if candidates, ok := usage["candidatesTokenCount"].(float64); ok {
			*outputTokens = int(candidates)
		}
	}

	// Process candidates
	if candidates, ok := event["candidates"].([]any); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]any); ok {
			if content, ok := candidate["content"].(map[string]any); ok {
				if parts, ok := content["parts"].([]any); ok {
					for _, part := range parts {
						if partMap, ok := part.(map[string]any); ok {
							if text, ok := partMap["text"].(string); ok {
								textContent.WriteString(text)
								if clientWantsStream {
									evt := map[string]any{
										"type": "response.output_text.delta",
										"delta": map[string]any{
											"type": "output_text_delta",
											"text": text,
										},
									}
									evtBytes, _ := json.Marshal(evt)
									*chunks = append(*chunks, []byte("event: response.output_text.delta\ndata: ")...)
									*chunks = append(*chunks, evtBytes...)
									*chunks = append(*chunks, []byte("\n\n")...)
								}
							}
						}
					}
				}
			}
			// Check for finish reason
			if finishReason, ok := candidate["finishReason"].(string); ok && finishReason != "" {
				if clientWantsStream {
					evt := map[string]any{
						"type": "response.completed",
						"response": map[string]any{
							"id":         responseID,
							"object":     "response",
							"created_at": time.Now().Unix(),
							"model":      *model,
							"status":     "completed",
						},
					}
					evtBytes, _ := json.Marshal(evt)
					*chunks = append(*chunks, []byte("event: response.completed\ndata: ")...)
					*chunks = append(*chunks, evtBytes...)
					*chunks = append(*chunks, []byte("\n\n")...)
				}
			}
		}
	}

	return nil
}
