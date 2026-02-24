// Package proxy provides the Codex upstream handler.
//
// Codex upstream workflow:
//  1. Read refresh_token from the JSON auth file
//  2. Refresh access_token via https://auth.openai.com/oauth/token
//  3. Parse account_id and email from the returned id_token (JWT)
//  4. Write updated tokens back to the JSON auth file
//  5. Send streaming request to https://chatgpt.com/backend-api/codex/responses
//     with Bearer token + Codex-specific headers
//  6. Parse SSE events (response.output_text.delta) for text output
package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/missdeer/aiproxy/config"
)

// ── OAuth / API constants ──────────────────────────────────────────────

const (
	codexTokenURL     = "https://auth.openai.com/oauth/token"
	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexRefreshScope = "openid profile email"
	codexBaseURL      = "https://chatgpt.com/backend-api/codex"
	codexVersion      = "0.101.0"
	codexUserAgent    = "codex_cli_rs/0.101.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464"
)

// ── Data structures ────────────────────────────────────────────────────

// CodexTokenStorage mirrors the on-disk JSON format for Codex auth.
type CodexTokenStorage struct {
	AccessToken  string `json:"access_token"`
	AccountID    string `json:"account_id"`
	Disabled     bool   `json:"disabled,omitempty"`
	Email        string `json:"email"`
	Expired      string `json:"expired"`
	IDToken      string `json:"id_token"`
	LastRefresh  string `json:"last_refresh"`
	RefreshToken string `json:"refresh_token"`
	Type         string `json:"type"`
}

// CodexTokenResponse is the response from the OAuth token endpoint.
type CodexTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// ── CodexAuth: manages token lifecycle for a single auth file ────────

// CodexAuth caches and auto-refreshes an OAuth token for a Codex auth file.
type CodexAuth struct {
	mu        sync.RWMutex
	authFile  string
	storage   *CodexTokenStorage
	expiresAt time.Time
	client    *http.Client
}

// NewCodexAuth creates a new CodexAuth for the given auth file path.
func NewCodexAuth(authFile string, timeout time.Duration) *CodexAuth {
	return &CodexAuth{
		authFile: authFile,
		client:   &http.Client{Timeout: timeout},
	}
}

// GetAccessToken returns a valid access token, refreshing if necessary.
func (ca *CodexAuth) GetAccessToken() (string, *CodexTokenStorage, error) {
	ca.mu.RLock()
	if ca.storage != nil && time.Now().Before(ca.expiresAt.Add(-60*time.Second)) {
		token := ca.storage.AccessToken
		storage := *ca.storage // copy
		ca.mu.RUnlock()
		return token, &storage, nil
	}
	ca.mu.RUnlock()

	ca.mu.Lock()
	defer ca.mu.Unlock()

	// Double check after acquiring write lock
	if ca.storage != nil && time.Now().Before(ca.expiresAt.Add(-60*time.Second)) {
		storage := *ca.storage
		return ca.storage.AccessToken, &storage, nil
	}

	// Load from file
	storage, err := codexLoadStorage(ca.authFile)
	if err != nil {
		return "", nil, fmt.Errorf("load auth file %s: %w", ca.authFile, err)
	}
	if storage.RefreshToken == "" {
		return "", nil, fmt.Errorf("refresh_token is empty in %s", ca.authFile)
	}

	// Refresh the token
	log.Printf("[CODEX] Refreshing token for %s (file: %s)", storage.Email, ca.authFile)
	tok, err := codexRefreshAccessToken(ca.client, storage.RefreshToken)
	if err != nil {
		return "", nil, fmt.Errorf("token refresh for %s: %w", ca.authFile, err)
	}

	// Update storage fields
	storage.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		storage.RefreshToken = tok.RefreshToken
	}
	if tok.IDToken != "" {
		storage.IDToken = tok.IDToken
		if acct, email, ok := codexParseIDToken(tok.IDToken); ok {
			if acct != "" {
				storage.AccountID = acct
			}
			if email != "" {
				storage.Email = email
			}
		}
	}
	storage.Expired = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339)
	storage.LastRefresh = time.Now().Format(time.RFC3339)

	// Save back to file
	if err := codexSaveStorage(ca.authFile, storage); err != nil {
		log.Printf("[CODEX] Warning: could not save updated tokens to %s: %v", ca.authFile, err)
	}

	ca.storage = storage
	ca.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	log.Printf("[CODEX] Token refreshed for %s, expires %s", storage.Email, storage.Expired)

	storageCopy := *storage
	return storage.AccessToken, &storageCopy, nil
}

// ── Global auth cache ──────────────────────────────────────────────────

var (
	codexAuthCacheMu sync.RWMutex
	codexAuthCache   = make(map[string]*CodexAuth)
)

func getCodexAuth(authFile string, timeout time.Duration) *CodexAuth {
	codexAuthCacheMu.RLock()
	auth, ok := codexAuthCache[authFile]
	codexAuthCacheMu.RUnlock()
	if ok {
		return auth
	}

	codexAuthCacheMu.Lock()
	defer codexAuthCacheMu.Unlock()

	// Double check
	if auth, ok := codexAuthCache[authFile]; ok {
		return auth
	}

	auth = NewCodexAuth(authFile, timeout)
	codexAuthCache[authFile] = auth
	return auth
}

// ── ForwardToCodex: called by other handlers' forwardRequest ──────────

// ForwardToCodex sends a request to the Codex upstream. The requestBody
// must already be in Responses API format (JSON). It returns the raw
// response from Codex (which uses Responses API SSE streaming format).
// The Codex API always uses SSE streaming; when clientWantsStream is false,
// the SSE output is reassembled into a single Responses-format JSON response.
func ForwardToCodex(client *http.Client, upstream config.Upstream, requestBody []byte, clientWantsStream bool) (int, []byte, http.Header, error) {
	if len(upstream.AuthFiles) == 0 {
		return 0, nil, nil, fmt.Errorf("codex upstream %s: auth_files is not configured", upstream.Name)
	}

	// Get timeout from client
	timeout := 60 * time.Second
	if client.Timeout > 0 {
		timeout = client.Timeout
	}

	// Get a valid access token (auto-refreshes if needed, round-robin across auth files)
	authFile := upstream.NextAuthFile()
	log.Printf("[CODEX] Using auth file: %s", authFile)
	auth := getCodexAuth(authFile, timeout)
	accessToken, storage, err := auth.GetAccessToken()
	if err != nil {
		return 0, nil, nil, fmt.Errorf("codex auth: %w", err)
	}

	// Ensure required fields for Codex API
	var bodyMap map[string]any
	if err := json.Unmarshal(requestBody, &bodyMap); err != nil {
		return 0, nil, nil, fmt.Errorf("parse request body: %w", err)
	}
	// Codex API always requires streaming
	bodyMap["stream"] = true
	bodyMap["store"] = false
	// Codex requires instructions field
	if _, ok := bodyMap["instructions"]; !ok {
		bodyMap["instructions"] = ""
	}
	// Codex requires input to be a list of message objects, not a plain string
	if inputStr, ok := bodyMap["input"].(string); ok {
		bodyMap["input"] = []map[string]any{
			{"role": "user", "content": inputStr},
		}
	}
	modifiedBody, err := json.Marshal(bodyMap)
	if err != nil {
		return 0, nil, nil, err
	}

	apiURL := codexBaseURL + "/responses"
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(modifiedBody))
	if err != nil {
		return 0, nil, nil, err
	}

	// Set Codex-specific headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("Version", codexVersion)
	req.Header.Set("Originator", "codex_cli_rs")
	req.Header.Set("Connection", "Keep-Alive")
	if storage.AccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", storage.AccountID)
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

	// If client doesn't want streaming, convert SSE to a single JSON response
	if !clientWantsStream && resp.StatusCode < 400 {
		jsonResp, err := codexSSEToResponsesJSON(respBody)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("codex SSE conversion: %w", err)
		}
		headers.Set("Content-Type", "application/json")
		return resp.StatusCode, jsonResp, headers, nil
	}

	return resp.StatusCode, respBody, headers, nil
}

// codexSSEToResponsesJSON parses Codex SSE output and assembles a non-streaming
// Responses API JSON response. The Codex API returns events such as
// response.output_text.delta, response.completed, etc.
func codexSSEToResponsesJSON(sseData []byte) ([]byte, error) {
	var (
		responseID  string
		model       string
		textContent strings.Builder
	)

	lines := strings.Split(string(sseData), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "" || data == "[DONE]" {
			continue
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "response.output_text.delta":
			if delta, ok := event["delta"].(string); ok {
				textContent.WriteString(delta)
			}
		case "response.completed":
			if resp, ok := event["response"].(map[string]any); ok {
				if id, ok := resp["id"].(string); ok {
					responseID = id
				}
				if m, ok := resp["model"].(string); ok {
					model = m
				}
			}
		}
	}

	// Build a Responses-format JSON response
	resp := map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": nil,
		"model":      model,
		"status":     "completed",
		"output": []map[string]any{
			{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{
						"type": "output_text",
						"text": textContent.String(),
					},
				},
				"status": "completed",
			},
		},
	}

	return json.Marshal(resp)
}

// ── Token refresh ──────────────────────────────────────────────────────

func codexRefreshAccessToken(client *http.Client, refreshTok string) (*CodexTokenResponse, error) {
	form := url.Values{
		"client_id":     {codexClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshTok},
		"scope":         {codexRefreshScope},
	}

	req, err := http.NewRequest("POST", codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var tok CodexTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &tok, nil
}

// ── JWT helpers (no signature verification) ────────────────────────────

func codexParseIDToken(idToken string) (accountID, email string, ok bool) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", "", false
	}
	payload, err := codexBase64URLDecode(parts[1])
	if err != nil {
		return "", "", false
	}

	var claims struct {
		Email    string `json:"email"`
		AuthInfo struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", "", false
	}
	return claims.AuthInfo.ChatGPTAccountID, claims.Email, true
}

func codexBase64URLDecode(s string) ([]byte, error) {
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.URLEncoding.DecodeString(s)
}

// ── File I/O ───────────────────────────────────────────────────────────

func codexLoadStorage(path string) (*CodexTokenStorage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s CodexTokenStorage
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func codexSaveStorage(path string, s *CodexTokenStorage) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}
