package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/missdeer/aiproxy/config"
)

func TestForwardToCodex_RequestCompression(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "codex-auth.json")
	storage := &CodexTokenStorage{
		AccessToken:  "cached-access-token",
		AccountID:    "acct-test",
		RefreshToken: "ref",
		Email:        "user@test.com",
	}
	data, _ := json.Marshal(storage)
	_ = os.WriteFile(authFile, data, 0644)

	codexAuthManager.StoreEntry(authFile, storage, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer codexAuthManager.DeleteEntry(authFile)

	var (
		capturedEncoding string
		capturedBody     []byte
	)
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				capturedEncoding = req.Header.Get("Content-Encoding")
				capturedBody, _ = io.ReadAll(req.Body)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(`data: {"type":"response.completed","response":{"id":"r1","model":"codex-mini"}}` + "\n"))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-codex",
		AuthFiles:          []string{authFile},
		RequestCompression: "gzip",
	}
	_, _, _, err := ForwardToCodex(client, upstream, []byte(`{"input":"`+strings.Repeat("hello-", 300)+`","model":"codex-mini-latest"}`), true)
	if err != nil {
		t.Fatalf("ForwardToCodex() error = %v", err)
	}
	if capturedEncoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want %q", capturedEncoding, "gzip")
	}

	decoded := mustGunzipForTest(t, capturedBody)
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("json.Unmarshal(decoded) error = %v", err)
	}
	if payload["stream"] != true {
		t.Fatalf("stream = %v, want true", payload["stream"])
	}
}

func TestForwardToClaudeCode_RequestCompression(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "claudecode-auth.json")
	storage := &ClaudeCodeTokenStorage{
		AccessToken:  "cached-token",
		RefreshToken: "refresh",
		Email:        "user@test.com",
	}
	data, _ := json.Marshal(storage)
	_ = os.WriteFile(authFile, data, 0644)

	claudeCodeAuthManager.StoreEntry(authFile, storage, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer claudeCodeAuthManager.DeleteEntry(authFile)

	var (
		capturedEncoding string
		capturedBody     []byte
	)
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				capturedEncoding = req.Header.Get("Content-Encoding")
				capturedBody, _ = io.ReadAll(req.Body)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4","content":[]}`))),
					Header:     http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-claudecode",
		AuthFiles:          []string{authFile},
		RequestCompression: "gzip",
	}
	requestBody := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"` + strings.Repeat("hello-", 300) + `"}]}`)
	_, _, _, err := ForwardToClaudeCode(client, upstream, requestBody, false)
	if err != nil {
		t.Fatalf("ForwardToClaudeCode() error = %v", err)
	}
	if capturedEncoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want %q", capturedEncoding, "gzip")
	}

	decoded := mustGunzipForTest(t, capturedBody)
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("json.Unmarshal(decoded) error = %v", err)
	}
	if payload["stream"] != false {
		t.Fatalf("stream = %v, want false", payload["stream"])
	}
}

func TestForwardToGeminiCLI_RequestCompression(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "geminicli-auth.json")
	storage := &GeminiCLITokenStorage{
		Token: map[string]any{
			"access_token":  "cached-token",
			"refresh_token": "refresh",
			"expiry":        time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
		ProjectID: "project-123",
		Email:     "user@test.com",
	}
	data, _ := json.Marshal(storage)
	_ = os.WriteFile(authFile, data, 0644)

	geminiCLIAuthManager.StoreEntry(authFile, storage, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer geminiCLIAuthManager.DeleteEntry(authFile)

	var (
		capturedEncoding string
		capturedBody     []byte
	)
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				capturedEncoding = req.Header.Get("Content-Encoding")
				capturedBody, _ = io.ReadAll(req.Body)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte("data: {}\n\n"))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-geminicli",
		AuthFiles:          []string{authFile},
		RequestCompression: "gzip",
	}
	_, _, _, err := ForwardToGeminiCLI(client, upstream, []byte(`{"input":"`+strings.Repeat("hello-", 300)+`","model":"gemini-3-pro-preview"}`), true)
	if err != nil {
		t.Fatalf("ForwardToGeminiCLI() error = %v", err)
	}
	if capturedEncoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want %q", capturedEncoding, "gzip")
	}

	decoded := mustGunzipForTest(t, capturedBody)
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("json.Unmarshal(decoded) error = %v", err)
	}
	if payload["project"] != "project-123" {
		t.Fatalf("project = %q, want %q", payload["project"], "project-123")
	}
}

func TestForwardToAntigravity_RequestCompression(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "antigravity-auth.json")
	storage := &AntigravityTokenStorage{
		AccessToken:  "cached-token",
		RefreshToken: "refresh",
		ProjectID:    "project-456",
		Email:        "user@test.com",
	}
	data, _ := json.Marshal(storage)
	_ = os.WriteFile(authFile, data, 0644)

	antigravityAuthManager.StoreEntry(authFile, storage, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer antigravityAuthManager.DeleteEntry(authFile)

	var (
		capturedEncoding string
		capturedBody     []byte
	)
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				capturedEncoding = req.Header.Get("Content-Encoding")
				capturedBody, _ = io.ReadAll(req.Body)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte("data: {}\n\n"))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-antigravity",
		AuthFiles:          []string{authFile},
		RequestCompression: "gzip",
	}
	_, _, _, err := ForwardToAntigravity(client, upstream, []byte(`{"input":"`+strings.Repeat("hello-", 300)+`","model":"gemini-2.5-flash"}`), true)
	if err != nil {
		t.Fatalf("ForwardToAntigravity() error = %v", err)
	}
	if capturedEncoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want %q", capturedEncoding, "gzip")
	}

	decoded := mustGunzipForTest(t, capturedBody)
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("json.Unmarshal(decoded) error = %v", err)
	}
	if payload["project"] != "project-456" {
		t.Fatalf("project = %q, want %q", payload["project"], "project-456")
	}
}

func mustGunzipForTest(t *testing.T, compressed []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	defer r.Close()
	decoded, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("io.ReadAll(gzip) error = %v", err)
	}
	return decoded
}
