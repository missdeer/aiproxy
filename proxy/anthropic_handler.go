package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/missdeer/aiproxy/balancer"
	"github.com/missdeer/aiproxy/config"
)

type AnthropicHandler struct {
	cfg      *config.Config
	balancer *balancer.WeightedRoundRobin
	client   *http.Client
	mu       sync.RWMutex
}

func NewAnthropicHandler(cfg *config.Config) *AnthropicHandler {
	timeout := time.Duration(cfg.UpstreamRequestTimeout) * time.Second
	return &AnthropicHandler{
		cfg:      cfg,
		balancer: balancer.NewWeightedRoundRobin(cfg.Upstreams),
		client:   &http.Client{Timeout: timeout},
	}
}

// UpdateConfig updates the handler's configuration
func (h *AnthropicHandler) UpdateConfig(cfg *config.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
	h.balancer.Update(cfg.Upstreams)
}

type MessageRequest struct {
	Model    string `json:"model"`
	Messages []any  `json:"messages"`
	Stream   bool   `json:"stream"`
}

func (h *AnthropicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	var req MessageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[ERROR] Invalid JSON request: %v", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Extract prompt preview for logging
	promptPreview := extractPromptPreview(req.Messages)
	log.Printf("[REQUEST] Model: %s, Stream: %v, Prompt: %s", req.Model, req.Stream, promptPreview)

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
		http.Error(w, fmt.Sprintf("No upstream supports model: %s", originalModel), http.StatusBadRequest)
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
		log.Printf("[FORWARD] Upstream: %s, URL: %s, Model: %s -> %s", upstream.Name, upstream.BaseURL, originalModel, mappedModel)

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

		// Success - copy response headers and body
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
	http.Error(w, fmt.Sprintf("All upstreams failed: %v", lastErr), lastStatus)
}

// extractPromptPreview extracts a preview of the prompt from messages
func extractPromptPreview(messages []any) string {
	if len(messages) == 0 {
		return "(empty)"
	}

	lastMsg := messages[len(messages)-1]
	msgMap, ok := lastMsg.(map[string]any)
	if !ok {
		return "(unknown format)"
	}

	content, ok := msgMap["content"]
	if !ok {
		return "(no content)"
	}

	var text string
	switch c := content.(type) {
	case string:
		text = c
	case []any:
		// Handle array of content blocks
		for _, block := range c {
			if blockMap, ok := block.(map[string]any); ok {
				if t, ok := blockMap["text"].(string); ok {
					text = t
					break
				}
			}
		}
	}

	return truncateString(text, 100)
}

// truncateString truncates a string to maxLen and adds "..." if truncated
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func (h *AnthropicHandler) forwardRequest(upstream config.Upstream, model string, originalBody []byte, clientWantsStream bool, originalReq *http.Request) (int, []byte, http.Header, error) {
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

	// Convert Anthropic request to target API format
	switch apiType {
	case config.APITypeCodex:
		// Convert Anthropic to Responses format, then forward to Codex
		responsesBody := convertAnthropicToResponsesRequest(bodyMap)
		modifiedBody, err = json.Marshal(responsesBody)
		if err != nil {
			return 0, nil, nil, err
		}
		// ForwardToCodex returns Responses-format data. Convert to Anthropic format.
		status, respBody, respHeaders, err := ForwardToCodex(h.client, upstream, modifiedBody, clientWantsStream)
		if err != nil {
			return status, nil, nil, err
		}
		if status >= 400 {
			return status, respBody, respHeaders, nil
		}
		if clientWantsStream {
			// Convert Codex SSE (Responses-format) to Anthropic streaming format
			anthropicResp, err := h.convertStreamToAnthropic(bytes.NewReader(respBody), config.APITypeResponses, false)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("failed to convert codex stream to anthropic: %w", err)
			}
			respHeaders.Set("Content-Type", "text/event-stream")
			return status, anthropicResp, respHeaders, nil
		}
		// Non-stream: convert Responses JSON to Anthropic format
		anthropicResp, err := convertResponseToAnthropic(respBody, config.APITypeResponses)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("failed to convert codex response to anthropic: %w", err)
		}
		respHeaders.Set("Content-Type", "application/json")
		return status, anthropicResp, respHeaders, nil

	case config.APITypeGeminiCLI:
		// Convert Anthropic to Responses format, then forward to Gemini CLI
		responsesBody := convertAnthropicToResponsesRequest(bodyMap)
		modifiedBody, err = json.Marshal(responsesBody)
		if err != nil {
			return 0, nil, nil, err
		}
		// ForwardToGeminiCLI returns Responses-format data. Convert to Anthropic format.
		status, respBody, respHeaders, err := ForwardToGeminiCLI(h.client, upstream, modifiedBody, clientWantsStream)
		if err != nil {
			return status, nil, nil, err
		}
		if status >= 400 {
			return status, respBody, respHeaders, nil
		}
		if clientWantsStream {
			// Convert Gemini CLI SSE (native Google format) to Anthropic streaming format
			anthropicResp, err := h.convertStreamToAnthropic(bytes.NewReader(respBody), config.APITypeGemini, false)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("failed to convert geminicli stream to anthropic: %w", err)
			}
			respHeaders.Set("Content-Type", "text/event-stream")
			return status, anthropicResp, respHeaders, nil
		}
		// Non-stream: convert Responses JSON to Anthropic format
		anthropicResp, err := convertResponseToAnthropic(respBody, config.APITypeResponses)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("failed to convert geminicli response to anthropic: %w", err)
		}
		respHeaders.Set("Content-Type", "application/json")
		return status, anthropicResp, respHeaders, nil

	case config.APITypeAntigravity:
		// Convert Anthropic to Responses format, then forward to Antigravity
		responsesBody := convertAnthropicToResponsesRequest(bodyMap)
		modifiedBody, err = json.Marshal(responsesBody)
		if err != nil {
			return 0, nil, nil, err
		}
		// ForwardToAntigravity returns Responses-format data. Convert to Anthropic format.
		status, respBody, respHeaders, err := ForwardToAntigravity(h.client, upstream, modifiedBody, clientWantsStream)
		if err != nil {
			return status, nil, nil, err
		}
		if status >= 400 {
			return status, respBody, respHeaders, nil
		}
		if clientWantsStream {
			// Convert Antigravity SSE (native Google format) to Anthropic streaming format
			anthropicResp, err := h.convertStreamToAnthropic(bytes.NewReader(respBody), config.APITypeGemini, false)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("failed to convert antigravity stream to anthropic: %w", err)
			}
			respHeaders.Set("Content-Type", "text/event-stream")
			return status, anthropicResp, respHeaders, nil
		}
		// Non-stream: convert Responses JSON to Anthropic format
		anthropicResp, err := convertResponseToAnthropic(respBody, config.APITypeResponses)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("failed to convert antigravity response to anthropic: %w", err)
		}
		respHeaders.Set("Content-Type", "application/json")
		return status, anthropicResp, respHeaders, nil

	case config.APITypeClaudeCode:
		// Forward to Claude Code (native Anthropic format)
		modifiedBody, err = json.Marshal(bodyMap)
		if err != nil {
			return 0, nil, nil, err
		}
		// ForwardToClaudeCode returns Anthropic-format data directly
		status, respBody, respHeaders, err := ForwardToClaudeCode(h.client, upstream, modifiedBody, clientWantsStream)
		if err != nil {
			return status, nil, nil, err
		}
		return status, respBody, respHeaders, nil

	case config.APITypeAnthropic:
		// Native Anthropic - no conversion needed
		modifiedBody, err = json.Marshal(bodyMap)
		if err != nil {
			return 0, nil, nil, err
		}
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/messages"
		// Pass through query parameters when API types match
		if originalReq.URL.RawQuery != "" {
			url = url + "?" + originalReq.URL.RawQuery
		}

	case config.APITypeOpenAI:
		// Convert Anthropic to OpenAI format
		openAIBody := convertAnthropicToOpenAIRequest(bodyMap)
		modifiedBody, err = json.Marshal(openAIBody)
		if err != nil {
			return 0, nil, nil, err
		}
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/chat/completions"

	case config.APITypeResponses:
		// Convert Anthropic to Responses format
		responsesBody := convertAnthropicToResponsesRequest(bodyMap)
		modifiedBody, err = json.Marshal(responsesBody)
		if err != nil {
			return 0, nil, nil, err
		}
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/responses"

	case config.APITypeGemini:
		// Convert Anthropic to Gemini format
		geminiBody := convertAnthropicToGeminiRequest(bodyMap)
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
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/messages"
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(modifiedBody))
	if err != nil {
		return 0, nil, nil, err
	}

	// Copy all headers from original request
	for k, vv := range originalReq.Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	// Remove hop-by-hop headers that should not be forwarded
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
	req.Header.Del("Content-Length") // Will be set automatically by http.Client

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	// Handle response conversion back to Anthropic format
	if resp.StatusCode < 400 && apiType != config.APITypeAnthropic {
		if clientWantsStream || forceStream {
			// Convert streaming response to Anthropic format
			anthropicResp, err := h.convertStreamToAnthropic(resp.Body, apiType, !clientWantsStream)
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
			return resp.StatusCode, anthropicResp, headers, nil
		} else {
			// Convert non-streaming response to Anthropic format
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return 0, nil, nil, err
			}
			anthropicResp, err := convertResponseToAnthropic(respBody, apiType)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("failed to convert response: %w", err)
			}
			headers := resp.Header.Clone()
			stripHopByHopHeaders(headers)
			headers.Del("Content-Length")
			headers.Set("Content-Type", "application/json")
			return resp.StatusCode, anthropicResp, headers, nil
		}
	}

	// If we forced streaming for Anthropic upstream, convert back to non-streaming
	if forceStream && resp.StatusCode < 400 && apiType == config.APITypeAnthropic {
		nonStreamResp, err := h.convertStreamToNonStream(resp.Body)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("failed to convert stream response: %w", err)
		}
		headers := resp.Header.Clone()
		stripHopByHopHeaders(headers)
		headers.Del("Content-Length")
		headers.Del("Content-Encoding")
		headers.Set("Content-Type", "application/json")
		return resp.StatusCode, nonStreamResp, headers, nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}

	return resp.StatusCode, respBody, resp.Header, nil
}

func (h *AnthropicHandler) streamResponse(w http.ResponseWriter, body []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Write(body)
		return
	}

	w.Write(body)
	flusher.Flush()
}

func stripHopByHopHeaders(h http.Header) {
	// https://www.rfc-editor.org/rfc/rfc2616#section-13.5.1 (obsoleted, but list remains applicable)
	if connection := h.Get("Connection"); connection != "" {
		for _, header := range strings.Split(connection, ",") {
			header = strings.TrimSpace(header)
			if header != "" {
				h.Del(header)
			}
		}
	}
	for _, header := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		h.Del(header)
	}
}

// convertStreamToNonStream reads SSE stream events and converts them to a non-streaming response
func (h *AnthropicHandler) convertStreamToNonStream(reader io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var response struct {
		ID           string `json:"id"`
		Type         string `json:"type"`
		Role         string `json:"role"`
		Content      []any  `json:"content"`
		Model        string `json:"model"`
		StopReason   string `json:"stop_reason,omitempty"`
		StopSequence string `json:"stop_sequence,omitempty"`
		Usage        struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	response.Type = "message"
	response.Role = "assistant"
	response.Content = make([]any, 0)

	// Track content blocks being built
	contentBlocks := make(map[int]map[string]any)
	var (
		currentEventType string
		dataBuilder      strings.Builder
		sawMessageStop   bool
	)

	flushEvent := func() error {
		if dataBuilder.Len() == 0 {
			return nil
		}
		data := dataBuilder.String()
		dataBuilder.Reset()

		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			return nil
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("invalid SSE JSON for event %q: %w (data=%s)", currentEventType, err, truncateString(data, 200))
		}

		eventType := currentEventType
		if eventType == "" {
			if t, ok := event["type"].(string); ok {
				eventType = t
			}
		}

		switch eventType {
		case "message_start":
			if msg, ok := event["message"].(map[string]any); ok {
				if id, ok := msg["id"].(string); ok {
					response.ID = id
				}
				if model, ok := msg["model"].(string); ok {
					response.Model = model
				}
				if usage, ok := msg["usage"].(map[string]any); ok {
					if input, ok := usage["input_tokens"].(float64); ok {
						response.Usage.InputTokens = int(input)
					}
				}
			}

		case "content_block_start":
			if index, ok := event["index"].(float64); ok {
				idx := int(index)
				if block, ok := event["content_block"].(map[string]any); ok {
					contentBlocks[idx] = block
				}
			}

		case "content_block_delta":
			if index, ok := event["index"].(float64); ok {
				idx := int(index)
				if delta, ok := event["delta"].(map[string]any); ok {
					if block, exists := contentBlocks[idx]; exists {
						// Append text delta
						if deltaType, ok := delta["type"].(string); ok && deltaType == "text_delta" {
							if text, ok := delta["text"].(string); ok {
								if existingText, ok := block["text"].(string); ok {
									block["text"] = existingText + text
								} else {
									block["text"] = text
								}
							}
						}
						// Handle thinking delta
						if deltaType, ok := delta["type"].(string); ok && deltaType == "thinking_delta" {
							if thinking, ok := delta["thinking"].(string); ok {
								if existingThinking, ok := block["thinking"].(string); ok {
									block["thinking"] = existingThinking + thinking
								} else {
									block["thinking"] = thinking
								}
							}
						}
					}
				}
			}

		case "message_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				if stopReason, ok := delta["stop_reason"].(string); ok {
					response.StopReason = stopReason
				}
				if stopSeq, ok := delta["stop_sequence"].(string); ok {
					response.StopSequence = stopSeq
				}
			}
			if usage, ok := event["usage"].(map[string]any); ok {
				if output, ok := usage["output_tokens"].(float64); ok {
					response.Usage.OutputTokens = int(output)
				}
			}

		case "message_stop":
			sawMessageStop = true

		case "error":
			if errObj, ok := event["error"].(map[string]any); ok {
				if msg, ok := errObj["message"].(string); ok && msg != "" {
					return fmt.Errorf("upstream sent error event: %s", msg)
				}
				if typ, ok := errObj["type"].(string); ok && typ != "" {
					return fmt.Errorf("upstream sent error event: %s", typ)
				}
			}
			return fmt.Errorf("upstream sent error event: %s", truncateString(data, 200))
		}

		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()

		// End of event.
		if line == "" {
			if err := flushEvent(); err != nil {
				return nil, err
			}
			if sawMessageStop {
				break
			}
			currentEventType = ""
			continue
		}

		// Comment line.
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse event type.
		if strings.HasPrefix(line, "event:") {
			currentEventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		// Parse data.
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			if dataBuilder.Len() > 0 {
				dataBuilder.WriteByte('\n')
			}
			dataBuilder.WriteString(data)
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flushEvent(); err != nil {
		return nil, err
	}

	// Build final content array from content blocks
	indexes := make([]int, 0, len(contentBlocks))
	for idx := range contentBlocks {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	for _, idx := range indexes {
		response.Content = append(response.Content, contentBlocks[idx])
	}

	return json.Marshal(response)
}
