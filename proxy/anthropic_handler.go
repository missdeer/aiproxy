package proxy

import (
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
		client:   newHTTPClient(timeout),
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
	log.Printf("[ANTHROPIC REQUEST] Model: %s, Stream: %v, Prompt: %s", req.Model, req.Stream, promptPreview)

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
		log.Printf("[FORWARD] Upstream: %s, URL: %s, Model: %s -> %s, APIType: %s",
			upstream.Name, upstream.BaseURL, originalModel, mappedModel, upstream.GetAPIType())

		status, respBody, respHeaders, streamResp, err := h.forwardRequest(upstream, mappedModel, body, req.Stream, r)
		if err != nil {
			log.Printf("[ERROR] Upstream %s connection error: %v", upstream.Name, err)
			if h.balancer.RecordFailure(upstream.Name, originalModel) {
				log.Printf("[CIRCUIT] Upstream %s model %s marked as unavailable after %d consecutive failures", upstream.Name, originalModel, 3)
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
			log.Printf("[ANTHROPIC] stream_start upstream=%s status=%d (headers sent to client)", upstream.Name, status)

			result := PipeStream(w, streamResp.Body)
			switch {
			case result.UpstreamErr != nil:
				if r.Context().Err() != nil {
					log.Printf("[INFO] Upstream %s stream aborted by client disconnect", upstream.Name)
				} else {
					log.Printf("[ERROR] Upstream %s stream read error: %v", upstream.Name, result.UpstreamErr)
					h.balancer.RecordFailure(upstream.Name, originalModel)
				}
			case result.DownstreamErr != nil:
				log.Printf("[INFO] Upstream %s stream ended by client disconnect: %v", upstream.Name, result.DownstreamErr)
			default:
				h.balancer.RecordSuccess(upstream.Name, originalModel)
				log.Printf("[SUCCESS] Upstream %s streaming completed (%d bytes, %s)", upstream.Name, result.BytesWritten, time.Since(streamStart))
			}
			return
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

func (h *AnthropicHandler) forwardRequest(upstream config.Upstream, model string, originalBody []byte, clientWantsStream bool, originalReq *http.Request) (int, []byte, http.Header, *http.Response, error) {
	var bodyMap map[string]any
	if err := json.Unmarshal(originalBody, &bodyMap); err != nil {
		return 0, nil, nil, nil, err
	}

	bodyMap["model"] = model

	apiType := upstream.GetAPIType()

	// Native Anthropic - direct passthrough without format conversion
	if apiType == config.APITypeAnthropic {
		modifiedBody, err := json.Marshal(bodyMap)
		if err != nil {
			return 0, nil, nil, nil, err
		}
		url := strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/messages"
		if originalReq.URL.RawQuery != "" {
			url = url + "?" + originalReq.URL.RawQuery
		}

		// Streaming: return raw response for direct pipe to client
		if clientWantsStream {
			resp, err := doHTTPRequestStream(h.client, url, modifiedBody, upstream, config.APITypeAnthropic, originalReq, "")
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
		status, respBody, headers, err := doHTTPRequest(h.client, url, modifiedBody, upstream, config.APITypeAnthropic, originalReq, "")
		if err != nil {
			return 0, nil, nil, nil, err
		}
		return status, respBody, headers, nil, nil
	}

	// Convert Anthropic request to canonical (Responses) format
	canonicalBody := convertAnthropicToResponsesRequest(bodyMap)
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

	// Streaming fast path: if sender supports SendStream and client wants streaming,
	// try to get the raw response for PipeStream passthrough.
	if clientWantsStream {
		if streamSender, ok := sender.(StreamCapableSender); ok {
			resp, respFormat, err := streamSender.SendStream(h.client, upstream, canonicalBytes, originalReq)
			if err != nil {
				return 0, nil, nil, nil, err
			}
			// Only use fast path if response format matches the client's protocol
			if respFormat == config.APITypeAnthropic {
				log.Printf("[ANTHROPIC] stream_mode=format_compatible_passthrough upstream=%s", upstream.Name)
				return HandleStreamResponse(resp)
			}
			// Format mismatch: read body and fall through to conversion path
			log.Printf("[ANTHROPIC] stream_mode=buffered_conversion upstream=%s respFormat=%s", upstream.Name, respFormat)
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
			anthropicResp, convErr := h.convertStreamToAnthropic(bytes.NewReader(respBody), respFormat, false)
			if convErr != nil {
				return 0, nil, nil, nil, fmt.Errorf("failed to convert stream response: %w", convErr)
			}
			headers.Set("Content-Type", "text/event-stream")
			return resp.StatusCode, anthropicResp, headers, nil, nil
		}
	}

	status, respBody, respHeaders, respFormat, err := sender.Send(h.client, upstream, canonicalBytes, clientWantsStream, originalReq)
	if err != nil {
		return status, nil, nil, nil, err
	}

	// Error responses pass through without conversion
	if status >= 400 {
		return status, respBody, respHeaders, nil, nil
	}

	// Convert response back to Anthropic format
	if clientWantsStream {
		anthropicResp, err := h.convertStreamToAnthropic(bytes.NewReader(respBody), respFormat, false)
		if err != nil {
			return 0, nil, nil, nil, fmt.Errorf("failed to convert stream response: %w", err)
		}
		respHeaders.Del("Content-Length")
		respHeaders.Del("Content-Encoding")
		respHeaders.Set("Content-Type", "text/event-stream")
		return status, anthropicResp, respHeaders, nil, nil
	}

	anthropicResp, err := convertResponseToAnthropic(respBody, respFormat)
	if err != nil {
		return 0, nil, nil, nil, fmt.Errorf("failed to convert response: %w", err)
	}
	respHeaders.Del("Content-Length")
	respHeaders.Set("Content-Type", "application/json")
	return status, anthropicResp, respHeaders, nil, nil
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
