// Package proxy provides the Claude Code upstream handler.
//
// Claude Code upstream workflow:
//  1. Read refresh_token from the JSON auth file
//  2. Refresh access_token via Anthropic's OAuth2 token endpoint
//  3. Send streaming request to https://api.anthropic.com/v1/messages
//  4. Parse SSE events to extract text from content_block_delta events
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/oauthcache"
	"github.com/missdeer/aiproxy/upstreammeta"
)

// ── OAuth / API constants ──────────────────────────────────────────────

const (
	claudeCodeTokenURL = "https://api.anthropic.com/v1/oauth/token"
	claudeCodeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeCodeBaseURL  = "https://api.anthropic.com"
	claudeCodeVersion  = "2023-06-01"
)

// ── Data structures ────────────────────────────────────────────────────

// ClaudeCodeTokenStorage mirrors the on-disk JSON format for Claude Code auth.
type ClaudeCodeTokenStorage struct {
	AccessToken  string `json:"access_token"`
	Disabled     bool   `json:"disabled,omitempty"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
	Expire       string `json:"expired"`
	LastRefresh  string `json:"last_refresh"`
	Type         string `json:"type"`
}

// ClaudeCodeTokenResponse is the response from the OAuth token endpoint.
type ClaudeCodeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Account      struct {
		EmailAddress string `json:"email_address"`
	} `json:"account"`
}

// ── ClaudeCode OAuth Manager (backed by oauthcache) ────────────────

var claudeCodeAuthManager = oauthcache.NewManager(oauthcache.Config[ClaudeCodeTokenStorage]{
	Label:          "[CLAUDECODE]",
	Load:           claudeCodeLoadStorage,
	Save:           claudeCodeSaveStorage,
	GetAccessToken: func(s *ClaudeCodeTokenStorage) string { return s.AccessToken },
	Copy:           func(s *ClaudeCodeTokenStorage) ClaudeCodeTokenStorage { return *s },
	Refresh:        claudeCodeRefreshAndUpdate,
	Disable: func(path string, s *ClaudeCodeTokenStorage) error {
		s.Disabled = true
		return claudeCodeSaveStorage(path, s)
	},
})

// claudeCodeRefreshAndUpdate performs the HTTP token refresh and updates storage in-place.
func claudeCodeRefreshAndUpdate(client *http.Client, storage *ClaudeCodeTokenStorage, authFile string) (time.Duration, error) {
	if storage.RefreshToken == "" {
		return 0, fmt.Errorf("refresh_token is empty in %s", authFile)
	}
	tok, statusCode, err := claudeCodeRefreshAccessToken(client, storage.RefreshToken)
	if err != nil {
		if statusCode == 401 || statusCode == 403 {
			return 0, &oauthcache.ErrAuthDisabled{AuthFile: authFile, Reason: err.Error()}
		}
		return 0, err
	}

	// Update storage fields
	storage.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		storage.RefreshToken = tok.RefreshToken
	}
	if tok.Account.EmailAddress != "" {
		storage.Email = tok.Account.EmailAddress
	}
	storage.Expire = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339)
	storage.LastRefresh = time.Now().Format(time.RFC3339)

	log.Printf("[CLAUDECODE] Token refreshed for %s, expires %s", storage.Email, storage.Expire)
	return time.Duration(tok.ExpiresIn) * time.Second, nil
}

// ── ForwardToClaudeCode: called by other handlers' forwardRequest ──────────

// ForwardToClaudeCode sends a request to the Claude Code upstream. The requestBody
// must already be in Anthropic Messages API format (JSON). It returns the raw
// response from Claude Code (which uses SSE streaming format).
// The Claude Code API supports both streaming and non-streaming; when clientWantsStream is false,
// the SSE output is reassembled into a single JSON response.
func ForwardToClaudeCode(client *http.Client, upstream config.Upstream, requestBody []byte, clientWantsStream bool) (int, []byte, http.Header, error) {
	// Get timeout from client
	timeout := ClientResponseHeaderTimeout(client)

	// Get a valid access token (auto-refreshes if needed, skips disabled files)
	accessToken, _, err := claudeCodeAuthManager.GetTokenWithRetry(
		upstream.AuthFiles, upstream.AuthFileStartIndex(), timeout, "[CLAUDECODE]")
	if err != nil {
		return 0, nil, nil, fmt.Errorf("claudecode auth: %w", err)
	}

	// Parse request body to ensure stream field is set correctly
	var bodyMap map[string]any
	if err := json.Unmarshal(requestBody, &bodyMap); err != nil {
		return 0, nil, nil, fmt.Errorf("parse request body: %w", err)
	}

	// Claude Code API supports both streaming and non-streaming
	bodyMap["stream"] = clientWantsStream

	modifiedBody, err := json.Marshal(bodyMap)
	if err != nil {
		return 0, nil, nil, err
	}

	apiURL := claudeCodeBaseURL + "/v1/messages?beta=true"
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(modifiedBody))
	if err != nil {
		return 0, nil, nil, err
	}

	// Set Claude Code-specific headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Anthropic-Version", claudeCodeVersion)
	req.Header.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14,prompt-caching-2024-07-31")
	req.Header.Set("User-Agent", upstreammeta.UserAgentClaudeCodeCLI)
	req.Header.Set("X-Stainless-Lang", "go")
	req.Header.Set("X-Stainless-Package-Version", "2.1.44")
	req.Header.Set("X-Stainless-Runtime", "go")
	req.Header.Set("X-Stainless-Runtime-Version", runtime.Version())
	req.Header.Set("X-Stainless-Arch", claudeCodeMapStainlessArch())
	req.Header.Set("X-Stainless-Os", claudeCodeMapStainlessOS())
	req.Header.Set("X-Stainless-Timeout", "600")
	req.Header.Set("Connection", "keep-alive")
	ApplyAcceptEncoding(req, upstream)
	if _, err := ApplyBodyCompression(req, modifiedBody, upstream); err != nil {
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
	StripHopByHopHeaders(headers)
	headers.Del("Content-Length")
	headers.Del("Content-Encoding")

	// If client doesn't want streaming but upstream returned streaming, convert SSE to JSON
	if !clientWantsStream && resp.StatusCode < 400 && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		jsonResp, err := claudeCodeSSEToJSON(respBody)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("claudecode SSE conversion: %w", err)
		}
		headers.Set("Content-Type", "application/json")
		return resp.StatusCode, jsonResp, headers, nil
	}

	return resp.StatusCode, respBody, headers, nil
}

// claudeCodeSSEToJSON parses Claude Code SSE output and assembles a non-streaming JSON response.
func claudeCodeSSEToJSON(sseData []byte) ([]byte, error) {
	scanner := bufio.NewScanner(bytes.NewReader(sseData))
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
			return fmt.Errorf("invalid SSE JSON: %w", err)
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
			}
			return fmt.Errorf("upstream sent error event")
		}

		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()

		// End of event
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

		// Comment line
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse event type
		if strings.HasPrefix(line, "event:") {
			currentEventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		// Parse data
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
	for i := 0; i < len(contentBlocks); i++ {
		if block, ok := contentBlocks[i]; ok {
			response.Content = append(response.Content, block)
		}
	}

	return json.Marshal(response)
}

// ── Token refresh ──────────────────────────────────────────────────────

func claudeCodeRefreshAccessToken(client *http.Client, refreshTok string) (*ClaudeCodeTokenResponse, int, error) {
	payload := map[string]string{
		"client_id":     claudeCodeClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshTok,
	}

	reqBody, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", claudeCodeTokenURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var tok ClaudeCodeTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse response: %w", err)
	}
	return &tok, resp.StatusCode, nil
}

// ── Stainless SDK helpers ──────────────────────────────────────────────

func claudeCodeMapStainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	default:
		return "Other::" + runtime.GOOS
	}
}

func claudeCodeMapStainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	default:
		return "other::" + runtime.GOARCH
	}
}

// ── File I/O ───────────────────────────────────────────────────────────

func claudeCodeLoadStorage(path string) (*ClaudeCodeTokenStorage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s ClaudeCodeTokenStorage
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Disabled {
		return nil, &oauthcache.ErrAuthDisabled{AuthFile: path, Reason: "marked disabled on disk"}
	}
	return &s, nil
}

func claudeCodeSaveStorage(path string, s *ClaudeCodeTokenStorage) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}
