package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"

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
		StripHopByHopHeaders(h)
		return resp.StatusCode, body, h, nil, nil
	}
	return resp.StatusCode, nil, nil, resp, nil
}

// doHTTPRequest builds and executes HTTP POST requests to the given URL.
// When the upstream has multiple tokens, it iterates them in round-robin
// order with failover on 4xx/5xx, mirroring the auth_files retry pattern.
func doHTTPRequest(client *http.Client, url string, body []byte, upstream config.Upstream, apiType config.APIType, originalReq *http.Request, rawQuery string) (int, []byte, http.Header, error) {
	tokens := upstream.Tokens
	if len(tokens) <= 1 {
		tok := ""
		if len(tokens) == 1 {
			tok = tokens[0]
		}
		return doHTTPRequestSingle(client, url, body, upstream, apiType, originalReq, rawQuery, tok)
	}

	start := upstream.TokenStartIndex()
	n := len(tokens)
	var lastStatus int
	var lastBody []byte
	var lastHeaders http.Header
	var lastErr error

	for i := 0; i < n; i++ {
		if err := originalReq.Context().Err(); err != nil {
			return 0, nil, nil, err
		}
		tok := tokens[(start+i)%n]
		status, respBody, headers, err := doHTTPRequestSingle(client, url, body, upstream, apiType, originalReq, rawQuery, tok)
		if err != nil {
			log.Printf("[TOKEN] upstream %q token %d/%d: %v", upstream.Name, i+1, n, err)
			lastErr = err
			lastStatus = http.StatusBadGateway
			continue
		}
		if status >= 400 {
			log.Printf("[TOKEN] upstream %q token %d/%d: HTTP %d", upstream.Name, i+1, n, status)
			lastStatus = status
			lastBody = respBody
			lastHeaders = headers
			lastErr = fmt.Errorf("upstream returned status %d", status)
			continue
		}
		return status, respBody, headers, nil
	}

	if lastBody != nil {
		return lastStatus, lastBody, lastHeaders, nil
	}
	return 0, nil, nil, lastErr
}

func doHTTPRequestSingle(client *http.Client, url string, body []byte, upstream config.Upstream, apiType config.APIType, originalReq *http.Request, rawQuery string, token string) (int, []byte, http.Header, error) {
	if rawQuery != "" {
		url = url + "?" + rawQuery
	}

	req, err := http.NewRequestWithContext(originalReq.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}

	for k, vv := range originalReq.Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	StripHopByHopHeaders(req.Header)

	switch apiType {
	case config.APITypeAnthropic:
		req.Header.Set("x-api-key", token)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Del("Authorization")
	case config.APITypeOpenAI, config.APITypeResponses:
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Del("x-api-key")
	case config.APITypeGemini:
		url = appendGeminiKey(url, token)
		req.URL, _ = req.URL.Parse(url)
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Del("Content-Length")
	ApplyAcceptEncoding(req, upstream)
	if _, err := ApplyBodyCompression(req, body, upstream); err != nil {
		return 0, nil, nil, fmt.Errorf("request body compression: %w", err)
	}
	ApplyCustomHeaders(req, upstream)

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
	StripHopByHopHeaders(headers)

	return resp.StatusCode, respBody, headers, nil
}

// doHTTPRequestStream builds and executes HTTP requests like doHTTPRequest,
// but returns the raw *http.Response without reading the body.
// When the upstream has multiple tokens, it iterates them with failover.
func doHTTPRequestStream(client *http.Client, url string, body []byte, upstream config.Upstream, apiType config.APIType, originalReq *http.Request, rawQuery string) (*http.Response, error) {
	tokens := upstream.Tokens
	if len(tokens) <= 1 {
		tok := ""
		if len(tokens) == 1 {
			tok = tokens[0]
		}
		return doHTTPRequestStreamSingle(client, url, body, upstream, apiType, originalReq, rawQuery, tok)
	}

	start := upstream.TokenStartIndex()
	n := len(tokens)
	var lastErr error
	var lastResp *http.Response

	for i := 0; i < n; i++ {
		if err := originalReq.Context().Err(); err != nil {
			if lastResp != nil {
				lastResp.Body.Close()
			}
			return nil, err
		}
		tok := tokens[(start+i)%n]
		resp, err := doHTTPRequestStreamSingle(client, url, body, upstream, apiType, originalReq, rawQuery, tok)
		if err != nil {
			log.Printf("[TOKEN] upstream %q token %d/%d (stream): %v", upstream.Name, i+1, n, err)
			lastErr = err
			continue
		}
		if resp.StatusCode >= 400 {
			log.Printf("[TOKEN] upstream %q token %d/%d (stream): HTTP %d", upstream.Name, i+1, n, resp.StatusCode)
			lastErr = fmt.Errorf("upstream returned status %d", resp.StatusCode)
			if lastResp != nil {
				lastResp.Body.Close()
			}
			lastResp = resp
			continue
		}
		if lastResp != nil {
			lastResp.Body.Close()
		}
		return resp, nil
	}

	if lastResp != nil {
		return lastResp, nil
	}
	return nil, lastErr
}

func doHTTPRequestStreamSingle(client *http.Client, url string, body []byte, upstream config.Upstream, apiType config.APIType, originalReq *http.Request, rawQuery string, token string) (*http.Response, error) {
	if rawQuery != "" {
		url = url + "?" + rawQuery
	}

	req, err := http.NewRequestWithContext(originalReq.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for k, vv := range originalReq.Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	StripHopByHopHeaders(req.Header)

	switch apiType {
	case config.APITypeAnthropic:
		req.Header.Set("x-api-key", token)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Del("Authorization")
	case config.APITypeOpenAI, config.APITypeResponses:
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Del("x-api-key")
	case config.APITypeGemini:
		url = appendGeminiKey(url, token)
		req.URL, _ = req.URL.Parse(url)
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Del("Content-Length")
	ApplyAcceptEncoding(req, upstream)
	if _, err := ApplyBodyCompression(req, body, upstream); err != nil {
		return nil, fmt.Errorf("request body compression: %w", err)
	}
	ApplyCustomHeaders(req, upstream)

	return client.Do(req)
}
