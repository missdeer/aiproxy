package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/missdeer/aiproxy/balancer"
	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/middleware"
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
	timeout := time.Duration(cfg.UpstreamRequestTimeout) * time.Second
	return BaseHandler{
		Cfg:      cfg,
		Balancer: balancer.NewWeightedRoundRobin(cfg.Upstreams),
		Client:   newHTTPClient(timeout),
	}
}

// newHTTPClient creates an http.Client that clones http.DefaultTransport and
// sets ResponseHeaderTimeout. This preserves env-proxy, HTTP/2, dial/TLS
// handshake defaults, and idle connection tuning from DefaultTransport.
func newHTTPClient(responseHeaderTimeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	transport.DisableCompression = true
	return &http.Client{
		Transport: &middleware.CompressedTransport{Base: transport},
	}
}

// ClientResponseHeaderTimeout extracts the ResponseHeaderTimeout from a client's transport.
// Falls back to 60s if not set.
func ClientResponseHeaderTimeout(client *http.Client) time.Duration {
	t := middleware.UnwrapTransport(client.Transport)
	if t != nil && t.ResponseHeaderTimeout > 0 {
		return t.ResponseHeaderTimeout
	}
	return 60 * time.Second
}

// ApplyAcceptEncoding sets or removes Accept-Encoding based on upstream compression config.
// Called by all outbound request builders to ensure consistent behavior.
func ApplyAcceptEncoding(req *http.Request, upstream config.Upstream) {
	if ae := upstream.GetAcceptEncoding(); ae != "" {
		req.Header.Set("Accept-Encoding", ae)
	} else {
		req.Header.Del("Accept-Encoding")
	}
}

// ApplyBodyCompression mutates request/body according to upstream request_compression.
// Returns the final body bytes actually attached to req.
func ApplyBodyCompression(req *http.Request, body []byte, upstream config.Upstream) ([]byte, error) {
	encoding := upstream.GetRequestContentEncoding()
	finalBody := body
	contentEncoding := ""
	if encoding != "" {
		compressed, ce, err := middleware.CompressBody(body, encoding)
		if err != nil {
			return nil, err
		}
		finalBody = compressed
		contentEncoding = ce
	}

	req.Body = io.NopCloser(bytes.NewReader(finalBody))
	req.ContentLength = int64(len(finalBody))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(finalBody)), nil
	}
	if contentEncoding == "" {
		req.Header.Del("Content-Encoding")
	} else {
		req.Header.Set("Content-Encoding", contentEncoding)
		log.Printf("[OUTBOUND COMPRESS] caller=upstream_request upstream=%s method=%s host=%s path=%s encoding=%s bytes=%d->%d",
			upstream.Name, req.Method, req.URL.Host, req.URL.Path, contentEncoding, len(body), len(finalBody))
	}
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(finalBody)))
	return finalBody, nil
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
		if u.SupportsModel(model) && b.Balancer.IsAvailable(u.Name, model) {
			supportedUpstreams = append(supportedUpstreams, u)
		}
	}

	if len(supportedUpstreams) == 0 {
		return nil, nil
	}

	// Use NextForModel to get the next upstream that supports this model
	next := b.Balancer.NextForModel(model)

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

// PipeResult reports the outcome of a PipeStream call.
type PipeResult struct {
	BytesWritten  int64
	UpstreamErr   error // non-nil if upstream (src.Read) failed
	DownstreamErr error // non-nil if downstream (w.Write) failed (client disconnect)
}

// OK returns true if streaming completed without error.
func (r PipeResult) OK() bool { return r.UpstreamErr == nil && r.DownstreamErr == nil }

// PipeStream copies from src to w with per-chunk flushing for true streaming.
// Uses a manual read/write/flush loop (NOT io.CopyBuffer) to guarantee
// each chunk is flushed immediately for incremental delivery.
func PipeStream(w http.ResponseWriter, src io.Reader) PipeResult {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			nw, writeErr := w.Write(buf[:n])
			total += int64(nw)
			if canFlush {
				flusher.Flush()
			}
			if writeErr != nil {
				return PipeResult{BytesWritten: total, DownstreamErr: writeErr}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return PipeResult{BytesWritten: total}
			}
			return PipeResult{BytesWritten: total, UpstreamErr: readErr}
		}
	}
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
