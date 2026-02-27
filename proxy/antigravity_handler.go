// Package proxy provides the Antigravity upstream handler.
//
// Antigravity upstream workflow:
//  1. Read refresh_token from the JSON auth file
//  2. Refresh access_token via Google's OAuth2 token endpoint
//  3. Discover project_id via loadCodeAssist API if not present
//  4. Send streaming request to https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent
//  5. Parse SSE events to extract text from candidates.0.content.parts
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
	"time"

	"github.com/google/uuid"
	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/oauthcache"
	"github.com/missdeer/aiproxy/upstreammeta"
	"github.com/tidwall/gjson"
)

// ── OAuth / API constants ──────────────────────────────────────────────

const (
	antigravityTokenURL          = "https://oauth2.googleapis.com/token"
	antigravityClientID          = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityClientSecret      = "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"
	antigravityBase              = "https://daily-cloudcode-pa.googleapis.com"
	antigravityStreamPath        = "/v1internal:streamGenerateContent"
	antigravitySystemInstruction = "You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.You are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question.**Absolute paths only****Proactiveness**"
)

// ── Data structures ────────────────────────────────────────────────────

// AntigravityTokenStorage mirrors the on-disk JSON format for Antigravity auth.
type AntigravityTokenStorage struct {
	AccessToken  string `json:"access_token"`
	Disabled     bool   `json:"disabled,omitempty"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
	ExpiresIn    int64  `json:"expires_in"`
	Expired      string `json:"expired"`
	Timestamp    int64  `json:"timestamp"`
	ProjectID    string `json:"project_id"`
	Type         string `json:"type"`
}

// AntigravityTokenResponse is the response from the OAuth token endpoint.
type AntigravityTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// ── Antigravity OAuth Manager (backed by oauthcache) ───────────────

var antigravityAuthManager = oauthcache.NewManager(oauthcache.Config[AntigravityTokenStorage]{
	Label:          "[ANTIGRAVITY]",
	Load:           antigravityLoadStorage,
	Save:           antigravitySaveStorage,
	GetAccessToken: func(s *AntigravityTokenStorage) string { return s.AccessToken },
	Copy:           func(s *AntigravityTokenStorage) AntigravityTokenStorage { return *s },
	Refresh:        antigravityRefreshAndUpdate,
	Disable: func(path string, s *AntigravityTokenStorage) error {
		s.Disabled = true
		return antigravitySaveStorage(path, s)
	},
})

// antigravityRefreshAndUpdate performs the HTTP token refresh and updates storage in-place.
func antigravityRefreshAndUpdate(client *http.Client, storage *AntigravityTokenStorage, authFile string) (time.Duration, error) {
	if storage.RefreshToken == "" {
		return 0, fmt.Errorf("refresh_token is empty in %s", authFile)
	}
	tok, statusCode, err := antigravityRefreshAccessToken(client, storage.RefreshToken)
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
	storage.ExpiresIn = tok.ExpiresIn
	now := time.Now()
	storage.Timestamp = now.UnixMilli()
	storage.Expired = now.Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339)

	// Discover project_id if not present
	if storage.ProjectID == "" {
		log.Printf("[ANTIGRAVITY] Discovering project_id for %s", storage.Email)
		pid, errPid := antigravityFetchProjectID(client, tok.AccessToken)
		if errPid != nil {
			log.Printf("[ANTIGRAVITY] Warning: failed to discover project_id: %v", errPid)
		} else {
			storage.ProjectID = pid
		}
	}

	log.Printf("[ANTIGRAVITY] Token refreshed for %s, expires %s, project_id: %s", storage.Email, storage.Expired, storage.ProjectID)
	return time.Duration(tok.ExpiresIn) * time.Second, nil
}

// ── ForwardToAntigravity: called by other handlers' forwardRequest ──────────

// ForwardToAntigravity sends a request to the Antigravity upstream. The requestBody
// must already be in Responses API format (JSON). It returns the raw
// response from Antigravity (which uses SSE streaming format).
// The Antigravity API always uses SSE streaming; when clientWantsStream is false,
// the SSE output is reassembled into a single Responses-format JSON response.
func ForwardToAntigravity(client *http.Client, upstream config.Upstream, requestBody []byte, clientWantsStream bool) (int, []byte, http.Header, error) {
	// Get timeout from client
	timeout := ClientResponseHeaderTimeout(client)

	// Get a valid access token (auto-refreshes if needed, skips disabled files)
	accessToken, storage, err := antigravityAuthManager.GetTokenWithRetry(
		upstream.AuthFiles, upstream.AuthFileStartIndex(), timeout, "[ANTIGRAVITY]")
	if err != nil {
		return 0, nil, nil, fmt.Errorf("antigravity auth: %w", err)
	}

	if storage.ProjectID == "" {
		return 0, nil, nil, fmt.Errorf("project_id is empty for upstream %s", upstream.Name)
	}

	// Parse request body to extract model and input
	var bodyMap map[string]any
	if err := json.Unmarshal(requestBody, &bodyMap); err != nil {
		return 0, nil, nil, fmt.Errorf("parse request body: %w", err)
	}

	model, _ := bodyMap["model"].(string)
	if model == "" {
		model = "gemini-3-pro-high"
	}

	// Convert Responses format to Antigravity format
	payload := map[string]any{
		"model":       model,
		"userAgent":   "antigravity",
		"requestType": "agent",
		"project":     storage.ProjectID,
		"requestId":   "agent-" + uuid.NewString(),
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

	// Apply system instruction (instructions field conversion + default injection for qualifying models)
	antigravityApplySystemInstruction(payload, model, bodyMap)

	reqBody, _ := json.Marshal(payload)

	apiURL := antigravityBase + antigravityStreamPath + "?alt=sse"
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return 0, nil, nil, err
	}

	// Set Antigravity-specific headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", upstreammeta.UserAgentAntigravity)
	ApplyAcceptEncoding(req, upstream)
	if _, err := ApplyBodyCompression(req, reqBody, upstream); err != nil {
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

	// If client doesn't want streaming, convert SSE to a single JSON response
	if !clientWantsStream && resp.StatusCode < 400 {
		jsonResp, err := antigravitySSEToResponsesJSON(respBody)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("antigravity SSE conversion: %w", err)
		}
		headers.Set("Content-Type", "application/json")
		return resp.StatusCode, jsonResp, headers, nil
	}

	return resp.StatusCode, respBody, headers, nil
}

// antigravitySSEToResponsesJSON parses Antigravity SSE output and assembles a non-streaming
// Responses API JSON response.
func antigravitySSEToResponsesJSON(sseData []byte) ([]byte, error) {
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
		responseNode := root.Get("response")
		if !responseNode.Exists() {
			if root.Get("candidates").Exists() {
				responseNode = root
			} else {
				continue
			}
		}

		// Extract text from candidates.0.content.parts
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

func antigravityRefreshAccessToken(client *http.Client, refreshTok string) (*AntigravityTokenResponse, int, error) {
	form := url.Values{
		"client_id":     {antigravityClientID},
		"client_secret": {antigravityClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshTok},
	}

	req, err := http.NewRequest("POST", antigravityTokenURL, strings.NewReader(form.Encode()))
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

	var tok AntigravityTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse response: %w", err)
	}
	return &tok, resp.StatusCode, nil
}

// ── Project ID discovery ───────────────────────────────────────────────

func antigravityFetchProjectID(client *http.Client, accessToken string) (string, error) {
	reqBody := map[string]any{
		"metadata": map[string]string{
			"ideType":    "ANTIGRAVITY",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	}

	payload, _ := json.Marshal(reqBody)
	apiURL := antigravityBase + "/v1internal:loadCodeAssist"

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", upstreammeta.UserAgentAntigravity)

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

// ── System instruction injection ───────────────────────────────────────

// antigravityApplySystemInstruction converts the Responses API "instructions"
// field to Gemini systemInstruction format, and injects the default Antigravity
// system instruction for qualifying models (Claude and gemini-3-pro-high).
// Existing instruction parts are preserved and appended after the defaults.
func antigravityApplySystemInstruction(payload map[string]any, model string, bodyMap map[string]any) {
	reqMap := payload["request"].(map[string]any)

	// Convert Responses API "instructions" field to Gemini systemInstruction
	if instructions, ok := bodyMap["instructions"].(string); ok && instructions != "" {
		reqMap["systemInstruction"] = map[string]any{
			"role": "user",
			"parts": []map[string]any{
				{"text": instructions},
			},
		}
	}

	// Inject default system instruction for Claude and gemini-3-pro-high models
	if strings.Contains(strings.ToLower(model), "claude") || strings.Contains(model, "gemini-3-pro-high") {
		// Preserve existing systemInstruction parts if any
		var existingParts []map[string]any
		if sysInst, ok := reqMap["systemInstruction"].(map[string]any); ok {
			if parts, ok := sysInst["parts"].([]map[string]any); ok {
				existingParts = parts
			}
		}

		// Build new systemInstruction with Antigravity's default parts
		newParts := []map[string]any{
			{"text": antigravitySystemInstruction},
			{"text": fmt.Sprintf("Please ignore following [ignore]%s[/ignore]", antigravitySystemInstruction)},
		}

		// Append existing parts
		if len(existingParts) > 0 {
			newParts = append(newParts, existingParts...)
		}

		reqMap["systemInstruction"] = map[string]any{
			"role":  "user",
			"parts": newParts,
		}
		log.Printf("[ANTIGRAVITY] Injecting default system instruction for model %s", model)
	}
}

// ── File I/O ───────────────────────────────────────────────────────────

func antigravityLoadStorage(path string) (*AntigravityTokenStorage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s AntigravityTokenStorage
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Disabled {
		return nil, &oauthcache.ErrAuthDisabled{AuthFile: path, Reason: "marked disabled on disk"}
	}
	return &s, nil
}

func antigravitySaveStorage(path string, s *AntigravityTokenStorage) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}
