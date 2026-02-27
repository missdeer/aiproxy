package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/oauthcache"
)

// ── codexBase64URLDecode tests ─────────────────────────────────────────

func TestCodexBase64URLDecode(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "no padding needed",
			input: base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte("hello")),
			want:  "hello",
		},
		{
			name:  "padding needed mod 2",
			input: base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte("hi")),
			want:  "hi",
		},
		{
			name:  "padding needed mod 3",
			input: base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte("abc")),
			want:  "abc",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := codexBase64URLDecode(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("codexBase64URLDecode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if string(got) != tt.want {
				t.Fatalf("codexBase64URLDecode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ── codexParseIDToken tests ────────────────────────────────────────────

func buildFakeJWT(payload any) string {
	header := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(`{"alg":"RS256"}`))
	body, _ := json.Marshal(payload)
	bodyEnc := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(body)
	sig := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte("signature"))
	return header + "." + bodyEnc + "." + sig
}

func TestCodexParseIDToken(t *testing.T) {
	t.Run("valid token with account and email", func(t *testing.T) {
		token := buildFakeJWT(map[string]any{
			"email": "user@example.com",
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "acct-123",
			},
		})
		acct, email, ok := codexParseIDToken(token)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if acct != "acct-123" {
			t.Fatalf("accountID = %q, want %q", acct, "acct-123")
		}
		if email != "user@example.com" {
			t.Fatalf("email = %q, want %q", email, "user@example.com")
		}
	})

	t.Run("missing account id", func(t *testing.T) {
		token := buildFakeJWT(map[string]any{
			"email": "user@example.com",
		})
		acct, email, ok := codexParseIDToken(token)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if acct != "" {
			t.Fatalf("accountID = %q, want empty", acct)
		}
		if email != "user@example.com" {
			t.Fatalf("email = %q, want %q", email, "user@example.com")
		}
	})

	t.Run("invalid token - not 3 parts", func(t *testing.T) {
		_, _, ok := codexParseIDToken("only.two")
		if ok {
			t.Fatal("expected ok=false for a 2-part token")
		}
	})

	t.Run("invalid base64 payload", func(t *testing.T) {
		_, _, ok := codexParseIDToken("aaa.!!!invalid.bbb")
		if ok {
			t.Fatal("expected ok=false for invalid base64")
		}
	})

	t.Run("invalid JSON payload", func(t *testing.T) {
		bad := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte("{not json"))
		_, _, ok := codexParseIDToken("aaa." + bad + ".bbb")
		if ok {
			t.Fatal("expected ok=false for invalid JSON")
		}
	})
}

// ── codexLoadStorage / codexSaveStorage tests ──────────────────────────

func TestCodexLoadAndSaveStorage(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	original := &CodexTokenStorage{
		AccessToken:  "access-tok",
		AccountID:    "acct-456",
		Email:        "test@example.com",
		RefreshToken: "refresh-tok",
		IDToken:      "id-tok",
		Expired:      "2026-12-31T00:00:00Z",
		LastRefresh:  "2026-01-01T00:00:00Z",
		Type:         "codex",
	}

	// Save
	if err := codexSaveStorage(authFile, original); err != nil {
		t.Fatalf("codexSaveStorage() error = %v", err)
	}

	// Load
	loaded, err := codexLoadStorage(authFile)
	if err != nil {
		t.Fatalf("codexLoadStorage() error = %v", err)
	}

	if loaded.AccessToken != original.AccessToken {
		t.Fatalf("AccessToken = %q, want %q", loaded.AccessToken, original.AccessToken)
	}
	if loaded.AccountID != original.AccountID {
		t.Fatalf("AccountID = %q, want %q", loaded.AccountID, original.AccountID)
	}
	if loaded.Email != original.Email {
		t.Fatalf("Email = %q, want %q", loaded.Email, original.Email)
	}
	if loaded.RefreshToken != original.RefreshToken {
		t.Fatalf("RefreshToken = %q, want %q", loaded.RefreshToken, original.RefreshToken)
	}
}

func TestCodexLoadStorage_FileNotFound(t *testing.T) {
	_, err := codexLoadStorage("/nonexistent/path/auth.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCodexLoadStorage_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "bad.json")
	if err := os.WriteFile(authFile, []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := codexLoadStorage(authFile)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestCodexSaveStorage_DisabledField(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "disabled.json")

	s := &CodexTokenStorage{
		AccessToken:  "tok",
		RefreshToken: "ref",
		Disabled:     true,
	}

	if err := codexSaveStorage(authFile, s); err != nil {
		t.Fatalf("codexSaveStorage() error = %v", err)
	}

	// codexLoadStorage should now return ErrAuthDisabled for a disabled file
	_, err := codexLoadStorage(authFile)
	if err == nil {
		t.Fatal("expected error when loading disabled file")
	}
	var disabledErr *oauthcache.ErrAuthDisabled
	if !errors.As(err, &disabledErr) {
		t.Fatalf("expected ErrAuthDisabled, got %T: %v", err, err)
	}
}

// ── codexSSEToResponsesJSON tests ──────────────────────────────────────

func TestCodexSSEToResponsesJSON(t *testing.T) {
	t.Run("assembles deltas and completed response", func(t *testing.T) {
		sse := "event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"Hello"}` + "\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":" World"}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp-123","model":"codex-mini-latest"}}` + "\n\n"

		result, err := codexSSEToResponsesJSON([]byte(sse))
		if err != nil {
			t.Fatalf("codexSSEToResponsesJSON() error = %v", err)
		}

		var resp map[string]any
		if err := json.Unmarshal(result, &resp); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}

		if resp["id"] != "resp-123" {
			t.Fatalf("id = %q, want %q", resp["id"], "resp-123")
		}
		if resp["model"] != "codex-mini-latest" {
			t.Fatalf("model = %q, want %q", resp["model"], "codex-mini-latest")
		}
		if resp["status"] != "completed" {
			t.Fatalf("status = %q, want %q", resp["status"], "completed")
		}

		output := resp["output"].([]any)
		if len(output) != 1 {
			t.Fatalf("output length = %d, want 1", len(output))
		}
		msg := output[0].(map[string]any)
		content := msg["content"].([]any)
		textObj := content[0].(map[string]any)
		if textObj["text"] != "Hello World" {
			t.Fatalf("text = %q, want %q", textObj["text"], "Hello World")
		}
	})

	t.Run("empty SSE data", func(t *testing.T) {
		result, err := codexSSEToResponsesJSON([]byte(""))
		if err != nil {
			t.Fatalf("codexSSEToResponsesJSON() error = %v", err)
		}
		var resp map[string]any
		if err := json.Unmarshal(result, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// Should produce a response with empty text
		output := resp["output"].([]any)
		msg := output[0].(map[string]any)
		content := msg["content"].([]any)
		textObj := content[0].(map[string]any)
		if textObj["text"] != "" {
			t.Fatalf("text = %q, want empty", textObj["text"])
		}
	})

	t.Run("handles DONE marker", func(t *testing.T) {
		sse := `data: {"type":"response.output_text.delta","delta":"Hi"}` + "\n" +
			"data: [DONE]\n"
		result, err := codexSSEToResponsesJSON([]byte(sse))
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		var resp map[string]any
		json.Unmarshal(result, &resp)
		output := resp["output"].([]any)
		msg := output[0].(map[string]any)
		content := msg["content"].([]any)
		textObj := content[0].(map[string]any)
		if textObj["text"] != "Hi" {
			t.Fatalf("text = %q, want %q", textObj["text"], "Hi")
		}
	})

	t.Run("skips invalid JSON lines", func(t *testing.T) {
		sse := "data: not-json\n" +
			`data: {"type":"response.output_text.delta","delta":"OK"}` + "\n"
		result, err := codexSSEToResponsesJSON([]byte(sse))
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		var resp map[string]any
		json.Unmarshal(result, &resp)
		output := resp["output"].([]any)
		msg := output[0].(map[string]any)
		content := msg["content"].([]any)
		textObj := content[0].(map[string]any)
		if textObj["text"] != "OK" {
			t.Fatalf("text = %q, want %q", textObj["text"], "OK")
		}
	})
}

// ── CodexAuth.GetAccessToken tests ─────────────────────────────────────

func TestCodexAuth_GetAccessToken_RefreshesAndCaches(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	// Build a fake JWT with account_id and email
	idToken := buildFakeJWT(map[string]any{
		"email": "test@codex.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-789",
		},
	})

	// Write initial auth file
	initial := &CodexTokenStorage{
		RefreshToken: "my-refresh-token",
		Email:        "old@codex.com",
	}
	data, _ := json.Marshal(initial)
	os.WriteFile(authFile, data, 0644)

	var refreshCallCount atomic.Int32

	ca := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				refreshCallCount.Add(1)
				tokResp := CodexTokenResponse{
					AccessToken:  "new-access-token",
					RefreshToken: "new-refresh-token",
					IDToken:      idToken,
					ExpiresIn:    3600,
				}
				body, _ := json.Marshal(tokResp)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
	}

	// Pre-store in cache for test
	codexAuthManager.StoreEntry(authFile, &CodexTokenStorage{
		RefreshToken: "my-refresh-token",
		Email:        "old@codex.com",
	}, time.Time{}, ca)
	defer codexAuthManager.DeleteEntry(authFile)

	// First call should trigger a refresh
	token1, stor1, err := codexAuthManager.GetToken(authFile, 30*time.Second)
	if err != nil {
		t.Fatalf("first GetAccessToken() error = %v", err)
	}
	if token1 != "new-access-token" {
		t.Fatalf("token = %q, want %q", token1, "new-access-token")
	}
	if stor1.Email != "test@codex.com" {
		t.Fatalf("email = %q, want %q", stor1.Email, "test@codex.com")
	}
	if stor1.AccountID != "acct-789" {
		t.Fatalf("accountID = %q, want %q", stor1.AccountID, "acct-789")
	}
	if c := refreshCallCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1", c)
	}

	// Second call should use cached token (no additional refresh)
	token2, _, err := codexAuthManager.GetToken(authFile, 30*time.Second)
	if err != nil {
		t.Fatalf("second GetAccessToken() error = %v", err)
	}
	if token2 != "new-access-token" {
		t.Fatalf("cached token = %q, want %q", token2, "new-access-token")
	}
	if c := refreshCallCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1 (should use cache)", c)
	}

	// Wait for async save goroutine to complete
	time.Sleep(100 * time.Millisecond)

	// Verify the saved file was updated
	saved, err := codexLoadStorage(authFile)
	if err != nil {
		t.Fatalf("codexLoadStorage() error = %v", err)
	}
	if saved.AccessToken != "new-access-token" {
		t.Fatalf("saved AccessToken = %q, want %q", saved.AccessToken, "new-access-token")
	}
	if saved.RefreshToken != "new-refresh-token" {
		t.Fatalf("saved RefreshToken = %q, want %q", saved.RefreshToken, "new-refresh-token")
	}
}

func TestCodexAuth_GetAccessToken_EmptyRefreshToken(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	// Write auth file with empty refresh token
	s := &CodexTokenStorage{Email: "test@example.com"}
	data, _ := json.Marshal(s)
	os.WriteFile(authFile, data, 0644)

	codexAuthManager.StoreEntry(authFile, &CodexTokenStorage{Email: "test@example.com"}, time.Time{}, &http.Client{Timeout: 30 * time.Second})
	defer codexAuthManager.DeleteEntry(authFile)
	_, _, err := codexAuthManager.GetToken(authFile, 30*time.Second)
	if err == nil {
		t.Fatal("expected error for empty refresh_token")
	}
}

func TestCodexAuth_GetAccessToken_FileNotFound(t *testing.T) {
	_, _, err := codexAuthManager.GetToken("/nonexistent/auth.json", 30*time.Second)
	if err == nil {
		t.Fatal("expected error for missing auth file")
	}
}

func TestCodexAuth_GetAccessToken_RefreshFails(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	s := &CodexTokenStorage{RefreshToken: "my-refresh"}
	data, _ := json.Marshal(s)
	os.WriteFile(authFile, data, 0644)

	ca := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"invalid_grant"}`))),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	codexAuthManager.StoreEntry(authFile, s, time.Time{}, ca)
	defer codexAuthManager.DeleteEntry(authFile)

	_, _, err := codexAuthManager.GetToken(authFile, 30*time.Second)
	if err == nil {
		t.Fatal("expected error when refresh fails")
	}
}

// ── ForwardToCodex tests ───────────────────────────────────────────────

func TestForwardToCodex_NoAuthFiles(t *testing.T) {
	upstream := config.Upstream{Name: "test-codex"}
	// AuthFiles is empty
	_, _, _, err := ForwardToCodex(&http.Client{}, upstream, []byte(`{}`), false)
	if err == nil {
		t.Fatal("expected error when auth_files is empty")
	}
}

func TestForwardToCodex_InputStringConversion(t *testing.T) {
	// This test verifies body modifications done by ForwardToCodex.
	// We set up a mock that captures the request body.
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	// Prepare a valid auth file with a cached (non-expired) token
	s := &CodexTokenStorage{
		AccessToken:  "cached-access-token",
		AccountID:    "acct-test",
		RefreshToken: "ref",
		Email:        "user@test.com",
	}
	data, _ := json.Marshal(s)
	os.WriteFile(authFile, data, 0644)

	// Pre-populate the auth cache so it doesn't actually try to refresh
	codexAuthManager.StoreEntry(authFile, s, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer func() {
		codexAuthManager.DeleteEntry(authFile)
	}()

	var capturedBody map[string]any

	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				bodyBytes, _ := io.ReadAll(req.Body)
				json.Unmarshal(bodyBytes, &capturedBody)

				// Return SSE response
				sse := `data: {"type":"response.output_text.delta","delta":"test"}` + "\n" +
					`data: {"type":"response.completed","response":{"id":"r1","model":"codex-mini"}}` + "\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(sse))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:      "test-codex",
		AuthFiles: []string{authFile},
	}

	requestBody := []byte(`{"input":"Hello, please help","model":"codex-mini-latest"}`)

	statusCode, respBody, _, err := ForwardToCodex(client, upstream, requestBody, false)
	if err != nil {
		t.Fatalf("ForwardToCodex() error = %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", statusCode)
	}

	// Verify input string was converted to message array
	inputArr, ok := capturedBody["input"].([]any)
	if !ok {
		t.Fatalf("input should be converted from string to array, got %T", capturedBody["input"])
	}
	if len(inputArr) != 1 {
		t.Fatalf("input array length = %d, want 1", len(inputArr))
	}
	msg := inputArr[0].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("role = %q, want %q", msg["role"], "user")
	}
	if msg["content"] != "Hello, please help" {
		t.Fatalf("content = %q, want %q", msg["content"], "Hello, please help")
	}

	// Verify stream and store flags set
	if capturedBody["stream"] != true {
		t.Fatalf("stream = %v, want true", capturedBody["stream"])
	}
	if capturedBody["store"] != false {
		t.Fatalf("store = %v, want false", capturedBody["store"])
	}

	// Verify instructions default was added
	if capturedBody["instructions"] != "" {
		t.Fatalf("instructions = %q, want empty string", capturedBody["instructions"])
	}

	// Verify truncation and context_management are not present
	if _, exists := capturedBody["truncation"]; exists {
		t.Fatal("truncation field should be removed for Codex compatibility")
	}
	if _, exists := capturedBody["context_management"]; exists {
		t.Fatal("context_management field should be removed for Codex compatibility")
	}

	// Since clientWantsStream=false, response should be assembled JSON
	var resp map[string]any
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["id"] != "r1" {
		t.Fatalf("response id = %q, want %q", resp["id"], "r1")
	}
}

func TestForwardToCodex_StreamingPassthrough(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	s := &CodexTokenStorage{
		AccessToken:  "tok",
		AccountID:    "acct",
		RefreshToken: "ref",
		Email:        "u@t.com",
	}
	data, _ := json.Marshal(s)
	os.WriteFile(authFile, data, 0644)

	codexAuthManager.StoreEntry(authFile, s, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer func() {
		codexAuthManager.DeleteEntry(authFile)
	}()

	rawSSE := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n"

	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(rawSSE))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:      "test-codex",
		AuthFiles: []string{authFile},
	}

	// clientWantsStream=true → raw SSE passthrough
	statusCode, respBody, _, err := ForwardToCodex(client, upstream, []byte(`{"input":"hi"}`), true)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", statusCode)
	}
	// Should return raw SSE, not assembled JSON
	if !bytes.Contains(respBody, []byte("response.output_text.delta")) {
		t.Fatal("expected raw SSE data in response")
	}
}

func TestForwardToCodex_StripsCompactFields(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	s := &CodexTokenStorage{
		AccessToken:  "cached-access-token",
		AccountID:    "acct-test",
		RefreshToken: "ref",
		Email:        "user@test.com",
	}
	data, _ := json.Marshal(s)
	os.WriteFile(authFile, data, 0644)

	codexAuthManager.StoreEntry(authFile, s, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer codexAuthManager.DeleteEntry(authFile)

	var capturedBody map[string]any

	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				bodyBytes, _ := io.ReadAll(req.Body)
				json.Unmarshal(bodyBytes, &capturedBody)
				sse := `data: {"type":"response.completed","response":{"id":"r1","model":"codex-mini"}}` + "\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(sse))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:      "test-codex",
		AuthFiles: []string{authFile},
	}

	requestBody := []byte(`{
		"input":"test",
		"model":"codex-mini-latest",
		"truncation":"disabled",
		"context_management":{"compaction":{"type":"compaction","compact_threshold":12000}}
	}`)

	_, _, _, err := ForwardToCodex(client, upstream, requestBody, false)
	if err != nil {
		t.Fatalf("ForwardToCodex() error = %v", err)
	}

	if _, exists := capturedBody["truncation"]; exists {
		t.Fatal("truncation field should be removed for Codex compatibility")
	}
	if _, exists := capturedBody["context_management"]; exists {
		t.Fatal("context_management field should be removed for Codex compatibility")
	}
}

func TestForwardToCodex_InvalidRequestBody(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	s := &CodexTokenStorage{
		AccessToken:  "tok",
		RefreshToken: "ref",
	}
	data, _ := json.Marshal(s)
	os.WriteFile(authFile, data, 0644)

	codexAuthManager.StoreEntry(authFile, s, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer func() {
		codexAuthManager.DeleteEntry(authFile)
	}()

	upstream := config.Upstream{
		Name:      "test-codex",
		AuthFiles: []string{authFile},
	}

	_, _, _, err := ForwardToCodex(&http.Client{}, upstream, []byte("not-json"), false)
	if err == nil {
		t.Fatal("expected error for invalid JSON body")
	}
}
