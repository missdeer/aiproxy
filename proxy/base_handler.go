package proxy

import (
	"net/http"
	"strings"
	"sync"

	"github.com/missdeer/aiproxy/balancer"
	"github.com/missdeer/aiproxy/config"
)

// BaseHandler contains shared fields and utilities for all handlers
type BaseHandler struct {
	Cfg      *config.Config
	Balancer *balancer.WeightedRoundRobin
	Client   *http.Client
	mu       sync.RWMutex
}

// NewBaseHandler creates a new BaseHandler with shared components
func NewBaseHandler(cfg *config.Config) BaseHandler {
	return BaseHandler{
		Cfg:      cfg,
		Balancer: balancer.NewWeightedRoundRobin(cfg.Upstreams),
		Client:   &http.Client{},
	}
}

// UpdateConfig updates the handler's configuration
func (b *BaseHandler) UpdateConfig(cfg *config.Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Cfg = cfg
	b.Balancer.Update(cfg.Upstreams)
}

// GetConfig returns the current configuration (thread-safe)
func (b *BaseHandler) GetConfig() *config.Config {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.Cfg
}

// FilterAndOrderUpstreams returns upstreams that support the given model,
// ordered starting from the next upstream in round-robin rotation
func (b *BaseHandler) FilterAndOrderUpstreams(model string) ([]config.Upstream, error) {
	upstreams := b.Balancer.GetAll()

	// Filter upstreams that support the requested model and are available
	var supportedUpstreams []config.Upstream
	for _, u := range upstreams {
		if u.SupportsModel(model) && b.Balancer.IsAvailable(u.Name) {
			supportedUpstreams = append(supportedUpstreams, u)
		}
	}

	if len(supportedUpstreams) == 0 {
		return nil, nil
	}

	// Reorder upstreams: start from the one returned by Next()
	next := b.Balancer.Next()
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

	return ordered, nil
}

// SetAuthHeaders sets authentication headers based on API type
func SetAuthHeaders(req *http.Request, upstream config.Upstream, url string) string {
	apiType := upstream.GetAPIType()

	switch apiType {
	case config.APITypeAnthropic:
		req.Header.Set("x-api-key", upstream.Token)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Del("Authorization")
	case config.APITypeGemini:
		if !strings.Contains(url, "key=") {
			url = url + "?key=" + upstream.Token
		}
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	default: // OpenAI and others
		req.Header.Set("Authorization", "Bearer "+upstream.Token)
		req.Header.Del("x-api-key")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Del("Content-Length")

	return url
}

// StreamResponse writes streaming response with flushing
func StreamResponse(w http.ResponseWriter, body []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Write(body)
		return
	}

	w.Write(body)
	flusher.Flush()
}

// StripHopByHopHeaders removes hop-by-hop headers that should not be forwarded
func StripHopByHopHeaders(h http.Header) {
	// https://www.rfc-editor.org/rfc/rfc2616#section-13.5.1
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

// TruncateString truncates a string to maxLen and adds "..." if truncated
func TruncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
