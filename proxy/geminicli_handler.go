// Package proxy provides the Gemini CLI upstream handler.
//
// Gemini CLI upstream workflow:
//  1. Read OAuth token from the JSON auth file
//  2. Refresh access_token via Google's OAuth2 token endpoint if needed
//  3. Discover project_id via loadCodeAssist API if not present
//  4. Send streaming request to https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent
//  5. Parse SSE events to extract text from response.candidates.0.content.parts
package proxy

import (
	"bytes"
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
	"github.com/missdeer/aiproxy/upstreammeta"
	"github.com/tidwall/gjson"
)

// ── OAuth / API constants ──────────────────────────────────────────────

const (
	geminiCLITokenURL     = "https://oauth2.googleapis.com/token"
	geminiCLIClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	geminiCLIClientSecret = "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"
	geminiCLIBase         = "https://cloudcode-pa.googleapis.com"
	geminiCLIPath         = "/v1internal:streamGenerateContent"
	geminiCLIAPIClient    = "gl-node/22.17.0"
)

// ── Data structures ────────────────────────────────────────────────────

// GeminiCLITokenStorage mirrors the on-disk JSON format for Gemini CLI auth.
type GeminiCLITokenStorage struct {
	Token     map[string]any `json:"token"`
	ProjectID string         `json:"project_id"`
	Email     string         `json:"email"`
	Type      string         `json:"type"`
}

// GeminiCLITokenResponse is the response from the OAuth token endpoint.
type GeminiCLITokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// ── GeminiCLIAuth: manages token lifecycle for a single auth file ────────

// GeminiCLIAuth caches and auto-refreshes an OAuth token for a Gemini CLI auth file.
type GeminiCLIAuth struct {
	mu        sync.RWMutex
	authFile  string
	storage   *GeminiCLITokenStorage
	expiresAt time.Time
	client    *http.Client
}

// NewGeminiCLIAuth creates a new GeminiCLIAuth for the given auth file path.
func NewGeminiCLIAuth(authFile string, timeout time.Duration) *GeminiCLIAuth {
	return &GeminiCLIAuth{
		authFile: authFile,
		client:   &http.Client{Timeout: timeout},
	}
}

// GetAccessToken returns a valid access token, refreshing if necessary.
func (ga *GeminiCLIAuth) GetAccessToken() (string, *GeminiCLITokenStorage, error) {
	ga.mu.RLock()
	if ga.storage != nil && time.Now().Before(ga.expiresAt.Add(-60*time.Second)) {
		token := geminiCLIStringValue(ga.storage.Token, "access_token")
		storage := *ga.storage // copy
		ga.mu.RUnlock()
		return token, &storage, nil
	}
	ga.mu.RUnlock()

	ga.mu.Lock()
	defer ga.mu.Unlock()

	// Double check after acquiring write lock
	if ga.storage != nil && time.Now().Before(ga.expiresAt.Add(-60*time.Second)) {
		storage := *ga.storage
		return geminiCLIStringValue(ga.storage.Token, "access_token"), &storage, nil
	}

	// Load from file
	storage, err := geminiCLILoadStorage(ga.authFile)
	if err != nil {
		return "", nil, fmt.Errorf("load auth file %s: %w", ga.authFile, err)
	}
	if storage.Token == nil {
		return "", nil, fmt.Errorf("token is empty in %s", ga.authFile)
	}

	// Extract current token
	accessToken := geminiCLIStringValue(storage.Token, "access_token")
	refreshToken := geminiCLIStringValue(storage.Token, "refresh_token")
	expiry := geminiCLIStringValue(storage.Token, "expiry")

	// Check if token is still valid
	if accessToken != "" && expiry != "" {
		if exp, err := time.Parse(time.RFC3339, expiry); err == nil {
			if time.Until(exp) > 5*time.Minute {
				// Discover project_id if not present (may have been missing when token was first cached)
				if storage.ProjectID == "" {
					log.Printf("[GEMINICLI] Discovering project_id for %s (cached token)", storage.Email)
					pid, errPid := geminiCLIFetchProjectID(ga.client, accessToken)
					if errPid != nil {
						log.Printf("[GEMINICLI] Warning: failed to discover project_id: %v", errPid)
						// Don't cache storage so next call retries discovery
						return accessToken, storage, nil
					}
					storage.ProjectID = pid
					if err := geminiCLISaveStorage(ga.authFile, storage); err != nil {
						log.Printf("[GEMINICLI] Warning: could not save project_id to %s: %v", ga.authFile, err)
					}
				}
				ga.storage = storage
				ga.expiresAt = exp
				return accessToken, storage, nil
			}
		}
	}

	// Refresh token
	if refreshToken == "" {
		return "", nil, fmt.Errorf("no refresh_token available in %s", ga.authFile)
	}

	log.Printf("[GEMINICLI] Refreshing token for %s (file: %s)", storage.Email, ga.authFile)
	tok, err := geminiCLIRefreshAccessToken(ga.client, refreshToken)
	if err != nil {
		return "", nil, fmt.Errorf("token refresh for %s: %w", ga.authFile, err)
	}

	// Update storage fields
	storage.Token["access_token"] = tok.AccessToken
	storage.Token["token_type"] = tok.TokenType
	if tok.RefreshToken != "" {
		storage.Token["refresh_token"] = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		expiry := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		storage.Token["expiry"] = expiry.Format(time.RFC3339)
		ga.expiresAt = expiry
	}

	// Discover project_id if not present
	if storage.ProjectID == "" {
		log.Printf("[GEMINICLI] Discovering project_id for %s", storage.Email)
		pid, errPid := geminiCLIFetchProjectID(ga.client, tok.AccessToken)
		if errPid != nil {
			log.Printf("[GEMINICLI] Warning: failed to discover project_id: %v", errPid)
		} else {
			storage.ProjectID = pid
		}
	}

	// Save back to file
	if err := geminiCLISaveStorage(ga.authFile, storage); err != nil {
		log.Printf("[GEMINICLI] Warning: could not save updated tokens to %s: %v", ga.authFile, err)
	}

	ga.storage = storage
	log.Printf("[GEMINICLI] Token refreshed for %s, project_id: %s", storage.Email, storage.ProjectID)

	storageCopy := *storage
	return tok.AccessToken, &storageCopy, nil
}

// ── Global auth cache ──────────────────────────────────────────────────

var (
	geminiCLIAuthCacheMu sync.RWMutex
	geminiCLIAuthCache   = make(map[string]*GeminiCLIAuth)
)

func getGeminiCLIAuth(authFile string, timeout time.Duration) *GeminiCLIAuth {
	geminiCLIAuthCacheMu.RLock()
	auth, ok := geminiCLIAuthCache[authFile]
	geminiCLIAuthCacheMu.RUnlock()
	if ok {
		return auth
	}

	geminiCLIAuthCacheMu.Lock()
	defer geminiCLIAuthCacheMu.Unlock()

	// Double check
	if auth, ok := geminiCLIAuthCache[authFile]; ok {
		return auth
	}

	auth = NewGeminiCLIAuth(authFile, timeout)
	geminiCLIAuthCache[authFile] = auth
	return auth
}

// ── ForwardToGeminiCLI: called by other handlers' forwardRequest ──────────

// ForwardToGeminiCLI sends a request to the Gemini CLI upstream. The requestBody
// must already be in Responses API format (JSON). It returns the raw
// response from Gemini CLI (which uses SSE streaming format).
// The Gemini CLI API always uses SSE streaming; when clientWantsStream is false,
// the SSE output is reassembled into a single Responses-format JSON response.
func ForwardToGeminiCLI(client *http.Client, upstream config.Upstream, requestBody []byte, clientWantsStream bool) (int, []byte, http.Header, error) {
	if len(upstream.AuthFiles) == 0 {
		return 0, nil, nil, fmt.Errorf("geminicli upstream %s: auth_files is not configured", upstream.Name)
	}

	// Get timeout from client
	timeout := 60 * time.Second
	if client.Timeout > 0 {
		timeout = client.Timeout
	}

	// Get a valid access token (auto-refreshes if needed, round-robin across auth files)
	authFile := upstream.NextAuthFile()
	log.Printf("[GEMINICLI] Using auth file: %s", authFile)
	auth := getGeminiCLIAuth(authFile, timeout)
	accessToken, storage, err := auth.GetAccessToken()
	if err != nil {
		return 0, nil, nil, fmt.Errorf("geminicli auth: %w", err)
	}

	if storage.ProjectID == "" {
		return 0, nil, nil, fmt.Errorf("project_id is empty for %s", authFile)
	}

	// Parse request body to extract model and input
	var bodyMap map[string]any
	if err := json.Unmarshal(requestBody, &bodyMap); err != nil {
		return 0, nil, nil, fmt.Errorf("parse request body: %w", err)
	}

	model, _ := bodyMap["model"].(string)
	if model == "" {
		model = "gemini-3-pro-preview"
	}

	// Convert Responses format to Gemini CLI format
	payload := map[string]any{
		"project": storage.ProjectID,
		"model":   model,
		"request": map[string]any{
			"contents": []map[string]any{},
		},
	}

	// Extract input from Responses format
	if input, ok := bodyMap["input"]; ok {
		switch inp := input.(type) {
		case string:
			payload["request"].(map[string]any)["contents"] = []map[string]any{
				{
					"role": "user",
					"parts": []map[string]any{
						{"text": inp},
					},
				},
			}
		case []any:
			// Already in message format
			contents := make([]map[string]any, 0, len(inp))
			for _, msg := range inp {
				if msgMap, ok := msg.(map[string]any); ok {
					role, _ := msgMap["role"].(string)
					content := msgMap["content"]

					parts := []map[string]any{}
					switch c := content.(type) {
					case string:
						parts = append(parts, map[string]any{"text": c})
					case []any:
						for _, part := range c {
							if partMap, ok := part.(map[string]any); ok {
								if text, ok := partMap["text"].(string); ok {
									parts = append(parts, map[string]any{"text": text})
								}
							}
						}
					}

					contents = append(contents, map[string]any{
						"role":  role,
						"parts": parts,
					})
				}
			}
			payload["request"].(map[string]any)["contents"] = contents
		}
	}

	reqBody, _ := json.Marshal(payload)

	apiURL := geminiCLIBase + geminiCLIPath + "?alt=sse"
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return 0, nil, nil, err
	}

	// Set Gemini CLI-specific headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", upstreammeta.UserAgentGeminiCLI)
	req.Header.Set("X-Goog-Api-Client", geminiCLIAPIClient)
	req.Header.Set("Client-Metadata", "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI")

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
		jsonResp, err := geminiCLISSEToResponsesJSON(respBody)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("geminicli SSE conversion: %w", err)
		}
		headers.Set("Content-Type", "application/json")
		return resp.StatusCode, jsonResp, headers, nil
	}

	return resp.StatusCode, respBody, headers, nil
}

// geminiCLISSEToResponsesJSON parses Gemini CLI SSE output and assembles a non-streaming
// Responses API JSON response.
func geminiCLISSEToResponsesJSON(sseData []byte) ([]byte, error) {
	var textContent strings.Builder

	lines := strings.Split(string(sseData), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		if data == "" || data == "[DONE]" {
			continue
		}

		// Parse JSON payload
		root := gjson.Parse(data)

		// Extract text from response.candidates.0.content.parts
		responseNode := root.Get("response")
		if !responseNode.Exists() {
			responseNode = root
		}

		parts := responseNode.Get("candidates.0.content.parts")
		if parts.IsArray() {
			for _, part := range parts.Array() {
				if text := part.Get("text").String(); text != "" {
					textContent.WriteString(text)
				}
			}
		}

		// Check for finish
		if finishReason := responseNode.Get("candidates.0.finishReason").String(); finishReason != "" {
			break
		}
	}

	// Build a Responses-format JSON response
	resp := map[string]any{
		"id":         "",
		"object":     "response",
		"created_at": nil,
		"model":      "",
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

func geminiCLIRefreshAccessToken(client *http.Client, refreshTok string) (*GeminiCLITokenResponse, error) {
	form := url.Values{
		"client_id":     {geminiCLIClientID},
		"client_secret": {geminiCLIClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshTok},
	}

	req, err := http.NewRequest("POST", geminiCLITokenURL, strings.NewReader(form.Encode()))
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

	var tok GeminiCLITokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &tok, nil
}

// ── Project ID discovery ───────────────────────────────────────────────

func geminiCLIFetchProjectID(client *http.Client, accessToken string) (string, error) {
	reqBody := map[string]any{
		"metadata": map[string]string{
			"ideType":    "IDE_UNSPECIFIED",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	}

	payload, _ := json.Marshal(reqBody)
	apiURL := geminiCLIBase + "/v1internal:loadCodeAssist"

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", upstreammeta.UserAgentGeminiCLI)
	req.Header.Set("X-Goog-Api-Client", geminiCLIAPIClient)
	req.Header.Set("Client-Metadata", "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if projectID, ok := result["cloudaicompanionProject"].(string); ok && projectID != "" {
		return projectID, nil
	}
	if projectMap, ok := result["cloudaicompanionProject"].(map[string]any); ok {
		if id, ok := projectMap["id"].(string); ok && id != "" {
			return id, nil
		}
	}

	return "", fmt.Errorf("no project ID found in response")
}

// ── File I/O ───────────────────────────────────────────────────────────

func geminiCLILoadStorage(path string) (*GeminiCLITokenStorage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s GeminiCLITokenStorage
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func geminiCLISaveStorage(path string, s *GeminiCLITokenStorage) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

// ── Helpers ────────────────────────────────────────────────────────────

func geminiCLIStringValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
