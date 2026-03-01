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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/oauthcache"
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
	Disabled  bool           `json:"disabled,omitempty"`
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

// ── GeminiCLI OAuth Manager (backed by oauthcache) ─────────────────

var geminiCLIAuthManager = oauthcache.NewManager(oauthcache.Config[GeminiCLITokenStorage]{
	Label: "[GEMINICLI]",
	Load:  geminiCLILoadStorage,
	Save:  geminiCLISaveStorage,
	GetAccessToken: func(s *GeminiCLITokenStorage) string {
		return geminiCLIStringValue(s.Token, "access_token")
	},
	Copy: func(s *GeminiCLITokenStorage) GeminiCLITokenStorage {
		c := *s
		c.Token = geminiCLICopyTokenMap(s.Token)
		return c
	},
	Refresh: geminiCLIRefreshAndUpdate,
	Disable: func(path string, s *GeminiCLITokenStorage) error {
		s.Disabled = true
		return geminiCLISaveStorage(path, s)
	},
})

// geminiCLIRefreshAndUpdate handles the full GeminiCLI refresh flow:
//  1. If the existing access_token is still valid (>5min to expiry), just discover ProjectID.
//  2. Otherwise, perform an HTTP token refresh.
func geminiCLIRefreshAndUpdate(client *http.Client, storage *GeminiCLITokenStorage, authFile string) (time.Duration, error) {
	if storage.Token == nil {
		return 0, fmt.Errorf("token is empty in %s", authFile)
	}

	accessToken := geminiCLIStringValue(storage.Token, "access_token")
	refreshToken := geminiCLIStringValue(storage.Token, "refresh_token")
	expiry := geminiCLIStringValue(storage.Token, "expiry")

	// Check if existing token is still valid
	if accessToken != "" && expiry != "" {
		if exp, err := time.Parse(time.RFC3339, expiry); err == nil {
			if time.Until(exp) > 5*time.Minute {
				// Token still valid, just discover project_id if needed
				if storage.ProjectID == "" {
					log.Printf("[GEMINICLI] Discovering project_id for %s (cached token)", storage.Email)
					pid, errPid := geminiCLIFetchProjectID(client, accessToken)
					if errPid != nil {
						log.Printf("[GEMINICLI] Warning: failed to discover project_id: %v", errPid)
						// Return 0 expiresIn so the manager won't cache, allowing retry next call
						return 0, nil
					}
					storage.ProjectID = pid
				}
				return time.Until(exp), nil
			}
		}
	}

	// Token expired or missing, refresh it
	if refreshToken == "" {
		return 0, fmt.Errorf("no refresh_token available in %s", authFile)
	}

	log.Printf("[GEMINICLI] Refreshing token for %s (file: %s)", storage.Email, authFile)
	tok, statusCode, err := geminiCLIRefreshAccessToken(client, refreshToken)
	if err != nil {
		if statusCode == 401 || statusCode == 403 {
			return 0, &oauthcache.ErrAuthDisabled{AuthFile: authFile, Reason: err.Error()}
		}
		return 0, err
	}

	// Update storage fields
	storage.Token["access_token"] = tok.AccessToken
	storage.Token["token_type"] = tok.TokenType
	if tok.RefreshToken != "" {
		storage.Token["refresh_token"] = tok.RefreshToken
	}
	var expiresIn time.Duration
	if tok.ExpiresIn > 0 {
		expiresIn = time.Duration(tok.ExpiresIn) * time.Second
		storage.Token["expiry"] = time.Now().Add(expiresIn).Format(time.RFC3339)
	}

	// Discover project_id if not present
	if storage.ProjectID == "" {
		log.Printf("[GEMINICLI] Discovering project_id for %s", storage.Email)
		pid, errPid := geminiCLIFetchProjectID(client, tok.AccessToken)
		if errPid != nil {
			log.Printf("[GEMINICLI] Warning: failed to discover project_id: %v", errPid)
		} else {
			storage.ProjectID = pid
		}
	}

	log.Printf("[GEMINICLI] Token refreshed for %s, expires %s, project_id: %s", storage.Email, time.Now().Add(expiresIn).Format(time.RFC3339), storage.ProjectID)
	return expiresIn, nil
}

// ── ForwardToGeminiCLI: called by other handlers' forwardRequest ──────────

// buildGeminiCLIBasePayload parses and transforms the request body in ways
// that are independent of which auth file is used. The "project" field
// is NOT set here; it is injected per-auth-file in the retry loop.
func buildGeminiCLIBasePayload(requestBody []byte) (map[string]any, string, error) {
	var bodyMap map[string]any
	if err := json.Unmarshal(requestBody, &bodyMap); err != nil {
		return nil, "", fmt.Errorf("parse request body: %w", err)
	}

	model, _ := bodyMap["model"].(string)
	if model == "" {
		model = "gemini-3-pro-preview"
	}

	payload := map[string]any{
		"model": model,
		"request": map[string]any{
			"contents": []map[string]any{},
		},
	}

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

	return payload, model, nil
}

// buildGeminiCLIRequest constructs the HTTP request for one auth file attempt.
func buildGeminiCLIRequest(upstream config.Upstream, body []byte, accessToken string, ctx context.Context) (*http.Request, error) {
	apiURL := geminiCLIBase + geminiCLIPath + "?alt=sse"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", upstreammeta.UserAgentGeminiCLI)
	req.Header.Set("X-Goog-Api-Client", geminiCLIAPIClient)
	req.Header.Set("Client-Metadata", "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI")
	ApplyAcceptEncoding(req, upstream)
	if _, err := ApplyBodyCompression(req, body, upstream); err != nil {
		return nil, fmt.Errorf("request body compression: %w", err)
	}
	return req, nil
}

// ForwardToGeminiCLI sends a request to the Gemini CLI upstream. The requestBody
// must already be in Responses API format (JSON). It returns the raw
// response from Gemini CLI (which uses SSE streaming format).
// The Gemini CLI API always uses SSE streaming; when clientWantsStream is false,
// the SSE output is reassembled into a single Responses-format JSON response.
func ForwardToGeminiCLI(client *http.Client, upstream config.Upstream, requestBody []byte, clientWantsStream bool) (int, []byte, http.Header, error) {
	basePayload, _, err := buildGeminiCLIBasePayload(requestBody)
	if err != nil {
		return 0, nil, nil, err
	}

	attempts, err := iterAuthFiles(upstream)
	if err != nil {
		return 0, nil, nil, err
	}

	timeout := ClientResponseHeaderTimeout(client)
	var lastErr error
	var lastStatus int
	var lastBody []byte
	var lastHeaders http.Header

	for i, attempt := range attempts {
		token, storage, err := geminiCLIAuthManager.GetToken(attempt.AuthFile, timeout)
		if err != nil {
			log.Printf("[GEMINICLI] Auth file %s (%d/%d): %v", attempt.AuthFile, i+1, len(attempts), err)
			lastErr = err
			continue
		}

		if storage.ProjectID == "" {
			log.Printf("[GEMINICLI] Auth file %s (%d/%d): project_id is empty", attempt.AuthFile, i+1, len(attempts))
			lastErr = fmt.Errorf("project_id is empty for auth file %s", attempt.AuthFile)
			continue
		}

		payload := clonePayload(basePayload)
		payload["project"] = storage.ProjectID

		body, err := json.Marshal(payload)
		if err != nil {
			lastErr = err
			continue
		}

		req, err := buildGeminiCLIRequest(upstream, body, token, context.Background())
		if err != nil {
			lastErr = err
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[GEMINICLI] Auth file %s (%d/%d): request failed: %v", attempt.AuthFile, i+1, len(attempts), err)
			lastErr = err
			lastStatus = http.StatusBadGateway
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			lastStatus = http.StatusBadGateway
			continue
		}

		headers := resp.Header.Clone()
		StripHopByHopHeaders(headers)
		headers.Del("Content-Length")
		headers.Del("Content-Encoding")

		if resp.StatusCode >= 400 {
			log.Printf("[GEMINICLI] Auth file %s (%d/%d): HTTP %d", attempt.AuthFile, i+1, len(attempts), resp.StatusCode)
			lastErr = fmt.Errorf("upstream returned status %d", resp.StatusCode)
			lastStatus = resp.StatusCode
			lastBody = respBody
			lastHeaders = headers
			continue
		}

		if !clientWantsStream {
			jsonResp, err := geminiCLISSEToResponsesJSON(respBody)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("geminicli SSE conversion: %w", err)
			}
			headers.Set("Content-Type", "application/json")
			return resp.StatusCode, jsonResp, headers, nil
		}
		return resp.StatusCode, respBody, headers, nil
	}

	if lastBody != nil {
		return lastStatus, lastBody, lastHeaders, nil
	}
	return lastStatus, nil, nil, lastErr
}

// ForwardToGeminiCLIStream sends a streaming request to the Gemini CLI upstream
// and returns the raw *http.Response with body unconsumed.
// The caller is responsible for closing resp.Body.
func ForwardToGeminiCLIStream(client *http.Client, upstream config.Upstream, requestBody []byte, ctx context.Context) (*http.Response, error) {
	basePayload, _, err := buildGeminiCLIBasePayload(requestBody)
	if err != nil {
		return nil, err
	}

	attempts, err := iterAuthFiles(upstream)
	if err != nil {
		return nil, err
	}

	timeout := ClientResponseHeaderTimeout(client)
	var lastErr error
	var lastResp *http.Response

	for i, attempt := range attempts {
		token, storage, err := geminiCLIAuthManager.GetToken(attempt.AuthFile, timeout)
		if err != nil {
			log.Printf("[GEMINICLI] Auth file %s (%d/%d): %v", attempt.AuthFile, i+1, len(attempts), err)
			lastErr = err
			continue
		}

		if storage.ProjectID == "" {
			log.Printf("[GEMINICLI] Auth file %s (%d/%d): project_id is empty", attempt.AuthFile, i+1, len(attempts))
			lastErr = fmt.Errorf("project_id is empty for auth file %s", attempt.AuthFile)
			continue
		}

		payload := clonePayload(basePayload)
		payload["project"] = storage.ProjectID

		body, err := json.Marshal(payload)
		if err != nil {
			lastErr = err
			continue
		}

		req, err := buildGeminiCLIRequest(upstream, body, token, ctx)
		if err != nil {
			lastErr = err
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[GEMINICLI] Auth file %s (%d/%d): request failed: %v", attempt.AuthFile, i+1, len(attempts), err)
			lastErr = err
			continue
		}

		if resp.StatusCode >= 400 {
			log.Printf("[GEMINICLI] Auth file %s (%d/%d): HTTP %d", attempt.AuthFile, i+1, len(attempts), resp.StatusCode)
			lastErr = fmt.Errorf("upstream returned status %d", resp.StatusCode)
			if lastResp != nil {
				lastResp.Body.Close()
			}
			lastResp = resp
			continue
		}

		if lastResp != nil {
			lastResp.Body.Close()
			lastResp = nil
		}
		return resp, nil
	}

	if lastResp != nil {
		return lastResp, nil
	}
	return nil, lastErr
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

func geminiCLIRefreshAccessToken(client *http.Client, refreshTok string) (*GeminiCLITokenResponse, int, error) {
	form := url.Values{
		"client_id":     {geminiCLIClientID},
		"client_secret": {geminiCLIClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshTok},
	}

	req, err := http.NewRequest("POST", geminiCLITokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

	var tok GeminiCLITokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse response: %w", err)
	}
	return &tok, resp.StatusCode, nil
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
	if s.Disabled {
		return nil, &oauthcache.ErrAuthDisabled{AuthFile: path, Reason: "marked disabled on disk"}
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

// geminiCLICopyTokenMap returns a shallow copy of the token map for safe async use.
func geminiCLICopyTokenMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	copy := make(map[string]any, len(m))
	for k, v := range m {
		copy[k] = v
	}
	return copy
}
