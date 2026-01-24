package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/missdeer/aiproxy/balancer"
	"github.com/missdeer/aiproxy/config"
)

type Handler struct {
	cfg      *config.Config
	balancer *balancer.WeightedRoundRobin
	client   *http.Client
}

func NewHandler(cfg *config.Config) *Handler {
	return &Handler{
		cfg:      cfg,
		balancer: balancer.NewWeightedRoundRobin(cfg.Upstreams),
		client:   &http.Client{},
	}
}

type MessageRequest struct {
	Model    string `json:"model"`
	Messages []any  `json:"messages"`
	Stream   bool   `json:"stream"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		if u.SupportsModel(originalModel) && h.balancer.IsAvailable(u.Name) {
			supportedUpstreams = append(supportedUpstreams, u)
		}
	}

	if len(supportedUpstreams) == 0 {
		log.Printf("[ERROR] No available upstream supports model: %s", originalModel)
		http.Error(w, fmt.Sprintf("No upstream supports model: %s", originalModel), http.StatusBadRequest)
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
		log.Printf("[FORWARD] Upstream: %s, URL: %s, Model: %s -> %s", upstream.Name, upstream.BaseURL, originalModel, mappedModel)

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

func (h *Handler) forwardRequest(upstream config.Upstream, model string, originalBody []byte, isStream bool, originalReq *http.Request) (int, []byte, http.Header, error) {
	var bodyMap map[string]any
	if err := json.Unmarshal(originalBody, &bodyMap); err != nil {
		return 0, nil, nil, err
	}

	bodyMap["model"] = model

	modifiedBody, err := json.Marshal(bodyMap)
	if err != nil {
		return 0, nil, nil, err
	}

	url := strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(modifiedBody))
	if err != nil {
		return 0, nil, nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", upstream.Token)
	req.Header.Set("anthropic-version", originalReq.Header.Get("anthropic-version"))

	if av := originalReq.Header.Get("anthropic-beta"); av != "" {
		req.Header.Set("anthropic-beta", av)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}

	return resp.StatusCode, respBody, resp.Header, nil
}

func (h *Handler) streamResponse(w http.ResponseWriter, body []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Write(body)
		return
	}

	w.Write(body)
	flusher.Flush()
}
