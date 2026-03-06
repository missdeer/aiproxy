package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/missdeer/aiproxy/config"
)

// OutboundSender sends requests to a specific upstream API type.
// It converts canonical (Responses-format) request body to the upstream's
// native format, builds the HTTP request, executes it, and returns the
// response along with its format for downstream conversion.
type OutboundSender interface {
	Send(client *http.Client, upstream config.Upstream, canonicalBody []byte, stream bool, originalReq *http.Request) (status int, body []byte, headers http.Header, respFormat config.APIType, err error)
}

// StreamCapableSender is optionally implemented by senders that can return
// an unread *http.Response for streaming passthrough. Handlers use type
// assertion to check for this capability.
type StreamCapableSender interface {
	SendStream(client *http.Client, upstream config.Upstream, canonicalBody []byte, originalReq *http.Request) (resp *http.Response, respFormat config.APIType, err error)
}

// OutboundSenderCreator is a factory function that creates a new OutboundSender.
type OutboundSenderCreator func() OutboundSender

// outboundSenderRegistry maps API types to their sender creators.
var outboundSenderRegistry = map[config.APIType]OutboundSenderCreator{}

// RegisterOutboundSender registers a sender creator for the given API type.
// It is typically called from init() in each sender's file.
func RegisterOutboundSender(apiType config.APIType, creator OutboundSenderCreator) {
	outboundSenderRegistry[apiType] = creator
}

// GetOutboundSender returns the appropriate OutboundSender for the given API type.
func GetOutboundSender(apiType config.APIType) OutboundSender {
	if creator, ok := outboundSenderRegistry[apiType]; ok {
		return creator()
	}
	// fallback: use Anthropic if no sender is registered
	if creator, ok := outboundSenderRegistry[config.APITypeAnthropic]; ok {
		return creator()
	}
	return nil
}

// ── Shared HTTP helper ─────────────────────────���────────────────────────

// HandleStreamResponse processes a raw *http.Response for streaming passthrough.
// On error status (>= 400), reads the body and returns buffered error response.
// On success, returns the unread response for PipeStream passthrough.
func HandleStreamResponse(resp *http.Response) (status int, errBody []byte, headers http.Header, streamResp *http.Response, err error) {
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return 0, nil, nil, nil, fmt.Errorf("failed to read upstream error response body: %w", readErr)
		}
		h := resp.Header.Clone()
		stripHopByHopHeaders(h)
		return resp.StatusCode, body, h, nil, nil
	}
	return resp.StatusCode, nil, nil, resp, nil
}

// doHTTPRequest builds and executes an HTTP POST request to the given URL
// with appropriate authentication headers based on the API type.
func doHTTPRequest(client *http.Client, url string, body []byte, upstream config.Upstream, apiType config.APIType, originalReq *http.Request, rawQuery string) (int, []byte, http.Header, error) {
	if rawQuery != "" {
		url = url + "?" + rawQuery
	}

	req, err := http.NewRequestWithContext(originalReq.Context(), http.MethodPost, url, bytes.NewReader(body))
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
			if strings.Contains(url, "?") {
				url = url + "&key=" + upstream.Token
			} else {
				url = url + "?key=" + upstream.Token
			}
			req.URL, _ = req.URL.Parse(url)
		}
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Del("Content-Length")
	ApplyAcceptEncoding(req, upstream)
	if _, err := ApplyBodyCompression(req, body, upstream); err != nil {
		return 0, nil, nil, fmt.Errorf("request body compression: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}

	headers := resp.Header.Clone()
	stripHopByHopHeaders(headers)

	return resp.StatusCode, respBody, headers, nil
}

// doHTTPRequestStream builds and executes an HTTP request like doHTTPRequest,
// but returns the raw *http.Response without reading the body.
// The caller is responsible for closing resp.Body.
// Uses originalReq.Context() for cancellation propagation.
func doHTTPRequestStream(client *http.Client, url string, body []byte, upstream config.Upstream, apiType config.APIType, originalReq *http.Request, rawQuery string) (*http.Response, error) {
	if rawQuery != "" {
		url = url + "?" + rawQuery
	}

	req, err := http.NewRequestWithContext(originalReq.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
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
			if strings.Contains(url, "?") {
				url = url + "&key=" + upstream.Token
			} else {
				url = url + "?key=" + upstream.Token
			}
			req.URL, _ = req.URL.Parse(url)
		}
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Del("Content-Length")
	ApplyAcceptEncoding(req, upstream)
	if _, err := ApplyBodyCompression(req, body, upstream); err != nil {
		return nil, fmt.Errorf("request body compression: %w", err)
	}

	return client.Do(req)
}
