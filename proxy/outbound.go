package proxy

import (
	"bytes"
	"encoding/json"
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

// GetOutboundSender returns the appropriate OutboundSender for the given API type.
func GetOutboundSender(apiType config.APIType) OutboundSender {
	switch apiType {
	case config.APITypeAnthropic:
		return &AnthropicSender{}
	case config.APITypeOpenAI:
		return &OpenAISender{}
	case config.APITypeGemini:
		return &GeminiSender{}
	case config.APITypeResponses:
		return &ResponsesSender{}
	case config.APITypeCodex:
		return &CodexSender{}
	case config.APITypeGeminiCLI:
		return &GeminiCLISender{}
	case config.APITypeAntigravity:
		return &AntigravitySender{}
	case config.APITypeClaudeCode:
		return &ClaudeCodeSender{}
	case config.APITypeKiro:
		return &KiroSender{}
	default:
		return &AnthropicSender{} // fallback
	}
}

// ── AnthropicSender ─────────────────────────────────────────────────────

type AnthropicSender struct{}

func (s *AnthropicSender) Send(client *http.Client, upstream config.Upstream, canonicalBody []byte, stream bool, originalReq *http.Request) (int, []byte, http.Header, config.APIType, error) {
	// Convert Responses format to Anthropic format
	var bodyMap map[string]any
	if err := json.Unmarshal(canonicalBody, &bodyMap); err != nil {
		return 0, nil, nil, "", err
	}

	anthropicBody := convertResponsesToAnthropicRequest(bodyMap)
	modifiedBody, err := json.Marshal(anthropicBody)
	if err != nil {
		return 0, nil, nil, "", err
	}

	url := strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/messages"

	status, respBody, headers, err := doHTTPRequest(client, url, modifiedBody, upstream, config.APITypeAnthropic, originalReq, "")
	if err != nil {
		return 0, nil, nil, "", err
	}
	return status, respBody, headers, config.APITypeAnthropic, nil
}

// ── OpenAISender ──��─────────────────────────────────────────────────────

type OpenAISender struct{}

func (s *OpenAISender) Send(client *http.Client, upstream config.Upstream, canonicalBody []byte, stream bool, originalReq *http.Request) (int, []byte, http.Header, config.APIType, error) {
	// Convert Responses format to OpenAI Chat format
	var bodyMap map[string]any
	if err := json.Unmarshal(canonicalBody, &bodyMap); err != nil {
		return 0, nil, nil, "", err
	}

	chatBody := convertResponsesToChatRequest(bodyMap)
	modifiedBody, err := json.Marshal(chatBody)
	if err != nil {
		return 0, nil, nil, "", err
	}

	url := strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/chat/completions"

	status, respBody, headers, err := doHTTPRequest(client, url, modifiedBody, upstream, config.APITypeOpenAI, originalReq, "")
	if err != nil {
		return 0, nil, nil, "", err
	}
	return status, respBody, headers, config.APITypeOpenAI, nil
}

// ── GeminiSender ────────────────────────────────────────────────────────

type GeminiSender struct{}

func (s *GeminiSender) Send(client *http.Client, upstream config.Upstream, canonicalBody []byte, stream bool, originalReq *http.Request) (int, []byte, http.Header, config.APIType, error) {
	// Convert Responses format to Gemini format
	var bodyMap map[string]any
	if err := json.Unmarshal(canonicalBody, &bodyMap); err != nil {
		return 0, nil, nil, "", err
	}

	model, _ := bodyMap["model"].(string)

	geminiBody := convertResponsesToGeminiRequest(bodyMap)
	modifiedBody, err := json.Marshal(geminiBody)
	if err != nil {
		return 0, nil, nil, "", err
	}

	action := "generateContent"
	if stream {
		action = "streamGenerateContent"
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:%s", strings.TrimSuffix(upstream.BaseURL, "/"), model, action)

	status, respBody, headers, err := doHTTPRequest(client, url, modifiedBody, upstream, config.APITypeGemini, originalReq, "")
	if err != nil {
		return 0, nil, nil, "", err
	}
	return status, respBody, headers, config.APITypeGemini, nil
}

// ── ResponsesSender ─────────────────────────────────────────────────────

type ResponsesSender struct{}

func (s *ResponsesSender) Send(client *http.Client, upstream config.Upstream, canonicalBody []byte, stream bool, originalReq *http.Request) (int, []byte, http.Header, config.APIType, error) {
	// Native Responses format - no conversion needed
	url := strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/responses"

	// Pass through query parameters for native format
	rawQuery := ""
	if originalReq.URL.RawQuery != "" {
		rawQuery = originalReq.URL.RawQuery
	}

	status, respBody, headers, err := doHTTPRequest(client, url, canonicalBody, upstream, config.APITypeResponses, originalReq, rawQuery)
	if err != nil {
		return 0, nil, nil, "", err
	}
	return status, respBody, headers, config.APITypeResponses, nil
}

// ── CodexSender ─────────────────────────────────────────────────────────

type CodexSender struct{}

func (s *CodexSender) Send(client *http.Client, upstream config.Upstream, canonicalBody []byte, stream bool, originalReq *http.Request) (int, []byte, http.Header, config.APIType, error) {
	// Codex uses Responses format natively, delegate to ForwardToCodex
	status, respBody, respHeaders, err := ForwardToCodex(client, upstream, canonicalBody, stream)
	if err != nil {
		return status, nil, nil, "", err
	}

	return status, respBody, respHeaders, config.APITypeResponses, nil
}

func (s *CodexSender) SendStream(client *http.Client, upstream config.Upstream, canonicalBody []byte, originalReq *http.Request) (*http.Response, config.APIType, error) {
	resp, err := ForwardToCodexStream(client, upstream, canonicalBody, originalReq.Context())
	if err != nil {
		return nil, "", err
	}
	return resp, config.APITypeResponses, nil
}

// ── GeminiCLISender ─────────────────────────────────────────────────────

type GeminiCLISender struct{}

func (s *GeminiCLISender) Send(client *http.Client, upstream config.Upstream, canonicalBody []byte, stream bool, originalReq *http.Request) (int, []byte, http.Header, config.APIType, error) {
	// GeminiCLI uses Responses format natively, delegate to ForwardToGeminiCLI
	status, respBody, respHeaders, err := ForwardToGeminiCLI(client, upstream, canonicalBody, stream)
	if err != nil {
		return status, nil, nil, "", err
	}

	// Streaming returns native Gemini SSE; non-streaming returns Responses JSON
	respFormat := config.APITypeResponses
	if stream {
		respFormat = config.APITypeGemini
	}
	return status, respBody, respHeaders, respFormat, nil
}

func (s *GeminiCLISender) SendStream(client *http.Client, upstream config.Upstream, canonicalBody []byte, originalReq *http.Request) (*http.Response, config.APIType, error) {
	resp, err := ForwardToGeminiCLIStream(client, upstream, canonicalBody, originalReq.Context())
	if err != nil {
		return nil, "", err
	}
	return resp, config.APITypeGemini, nil
}

// ── AntigravitySender ───────────────────────────────────────────────────

type AntigravitySender struct{}

func (s *AntigravitySender) Send(client *http.Client, upstream config.Upstream, canonicalBody []byte, stream bool, originalReq *http.Request) (int, []byte, http.Header, config.APIType, error) {
	// Antigravity uses Responses format natively, delegate to ForwardToAntigravity
	status, respBody, respHeaders, err := ForwardToAntigravity(client, upstream, canonicalBody, stream)
	if err != nil {
		return status, nil, nil, "", err
	}

	// Streaming returns native Gemini SSE; non-streaming returns Responses JSON
	respFormat := config.APITypeResponses
	if stream {
		respFormat = config.APITypeGemini
	}
	return status, respBody, respHeaders, respFormat, nil
}

func (s *AntigravitySender) SendStream(client *http.Client, upstream config.Upstream, canonicalBody []byte, originalReq *http.Request) (*http.Response, config.APIType, error) {
	resp, err := ForwardToAntigravityStream(client, upstream, canonicalBody, originalReq.Context())
	if err != nil {
		return nil, "", err
	}
	return resp, config.APITypeGemini, nil
}

// ── ClaudeCodeSender ────────────────────────────────────────────────────

type ClaudeCodeSender struct{}

func (s *ClaudeCodeSender) Send(client *http.Client, upstream config.Upstream, canonicalBody []byte, stream bool, originalReq *http.Request) (int, []byte, http.Header, config.APIType, error) {
	// Convert Responses format to Anthropic format for Claude Code
	var bodyMap map[string]any
	if err := json.Unmarshal(canonicalBody, &bodyMap); err != nil {
		return 0, nil, nil, "", err
	}

	anthropicBody := convertResponsesToAnthropicRequest(bodyMap)
	modifiedBody, err := json.Marshal(anthropicBody)
	if err != nil {
		return 0, nil, nil, "", err
	}

	// ForwardToClaudeCode returns Anthropic-format data
	status, respBody, respHeaders, err := ForwardToClaudeCode(client, upstream, modifiedBody, stream)
	if err != nil {
		return status, nil, nil, "", err
	}
	return status, respBody, respHeaders, config.APITypeAnthropic, nil
}

func (s *ClaudeCodeSender) SendStream(client *http.Client, upstream config.Upstream, canonicalBody []byte, originalReq *http.Request) (*http.Response, config.APIType, error) {
	// Convert Responses format to Anthropic format for Claude Code
	var bodyMap map[string]any
	if err := json.Unmarshal(canonicalBody, &bodyMap); err != nil {
		return nil, "", err
	}

	anthropicBody := convertResponsesToAnthropicRequest(bodyMap)
	modifiedBody, err := json.Marshal(anthropicBody)
	if err != nil {
		return nil, "", err
	}

	resp, err := ForwardToClaudeCodeStream(client, upstream, modifiedBody, originalReq.Context())
	if err != nil {
		return nil, "", err
	}
	return resp, config.APITypeAnthropic, nil
}

// ── KiroSender ────────────────────────────────────────────────────────

type KiroSender struct{}

func (s *KiroSender) Send(client *http.Client, upstream config.Upstream, canonicalBody []byte, stream bool, originalReq *http.Request) (int, []byte, http.Header, config.APIType, error) {
	status, respBody, respHeaders, err := ForwardToKiro(client, upstream, canonicalBody, stream)
	if err != nil {
		return status, nil, nil, "", err
	}
	return status, respBody, respHeaders, config.APITypeAnthropic, nil
}

func (s *KiroSender) SendStream(client *http.Client, upstream config.Upstream, canonicalBody []byte, originalReq *http.Request) (*http.Response, config.APIType, error) {
	resp, err := ForwardToKiroStream(client, upstream, canonicalBody, originalReq.Context())
	if err != nil {
		return nil, "", err
	}
	return resp, config.APITypeAnthropic, nil
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
