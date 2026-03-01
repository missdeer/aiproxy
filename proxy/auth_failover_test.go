package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/missdeer/aiproxy/config"
)

func TestIterAuthFiles_Empty(t *testing.T) {
	upstream := config.Upstream{Name: "test"}
	_, err := iterAuthFiles(upstream)
	if err == nil {
		t.Fatal("expected error for empty AuthFiles")
	}
}

func TestIterAuthFiles_Single(t *testing.T) {
	upstream := config.Upstream{
		Name:      "test",
		AuthFiles: []string{"/a.json"},
	}
	attempts, err := iterAuthFiles(upstream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(attempts))
	}
	if attempts[0].AuthFile != "/a.json" {
		t.Fatalf("AuthFile = %q, want %q", attempts[0].AuthFile, "/a.json")
	}
	if !attempts[0].IsLast {
		t.Fatal("single attempt should be IsLast")
	}
}

func TestIterAuthFiles_Multiple(t *testing.T) {
	upstream := config.Upstream{
		Name:      "test",
		AuthFiles: []string{"/a.json", "/b.json", "/c.json"},
	}
	attempts, err := iterAuthFiles(upstream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(attempts))
	}
	if !attempts[2].IsLast {
		t.Fatal("last attempt should be IsLast")
	}
	if attempts[0].IsLast || attempts[1].IsLast {
		t.Fatal("non-last attempts should not be IsLast")
	}
}

func TestClonePayload(t *testing.T) {
	orig := map[string]any{
		"model":   "test",
		"request": map[string]any{"contents": []any{}},
	}
	clone := clonePayload(orig)
	clone["project"] = "proj-123"
	if _, ok := orig["project"]; ok {
		t.Fatal("clone mutation leaked to original")
	}
}

func TestForwardToCodex_AuthFileFailover_FirstFails_SecondSucceeds(t *testing.T) {
	tmpDir := t.TempDir()
	authFile1 := filepath.Join(tmpDir, "auth1.json")
	authFile2 := filepath.Join(tmpDir, "auth2.json")

	s1 := &CodexTokenStorage{
		AccessToken:  "tok1",
		AccountID:    "acct1",
		RefreshToken: "ref1",
		Email:        "u1@test.com",
	}
	s2 := &CodexTokenStorage{
		AccessToken:  "tok2",
		AccountID:    "acct2",
		RefreshToken: "ref2",
		Email:        "u2@test.com",
	}
	d1, _ := json.Marshal(s1)
	d2, _ := json.Marshal(s2)
	os.WriteFile(authFile1, d1, 0644)
	os.WriteFile(authFile2, d2, 0644)

	codexAuthManager.StoreEntry(authFile1, s1, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	codexAuthManager.StoreEntry(authFile2, s2, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer codexAuthManager.DeleteEntry(authFile1)
	defer codexAuthManager.DeleteEntry(authFile2)

	var callCount atomic.Int32
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				n := callCount.Add(1)
				if n == 1 {
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"rate_limited"}`))),
						Header:     http.Header{"Content-Type": {"application/json"}},
					}, nil
				}
				sse := `data: {"type":"response.output_text.delta","delta":"OK"}` + "\n" +
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
		Name:               "test-codex",
		AuthFiles:          []string{authFile1, authFile2},
		RequestCompression: "none",
	}

	statusCode, respBody, _, err := ForwardToCodex(client, upstream, []byte(`{"input":"hello"}`), false)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", statusCode)
	}
	if callCount.Load() != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d", callCount.Load())
	}
	var resp map[string]any
	json.Unmarshal(respBody, &resp)
	if resp["id"] != "r1" {
		t.Fatalf("response id = %q, want %q", resp["id"], "r1")
	}
}

func TestForwardToCodex_AllAuthFilesFail(t *testing.T) {
	tmpDir := t.TempDir()
	authFile1 := filepath.Join(tmpDir, "auth1.json")
	authFile2 := filepath.Join(tmpDir, "auth2.json")

	s1 := &CodexTokenStorage{AccessToken: "tok1", RefreshToken: "ref1"}
	s2 := &CodexTokenStorage{AccessToken: "tok2", RefreshToken: "ref2"}
	d1, _ := json.Marshal(s1)
	d2, _ := json.Marshal(s2)
	os.WriteFile(authFile1, d1, 0644)
	os.WriteFile(authFile2, d2, 0644)

	codexAuthManager.StoreEntry(authFile1, s1, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	codexAuthManager.StoreEntry(authFile2, s2, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer codexAuthManager.DeleteEntry(authFile1)
	defer codexAuthManager.DeleteEntry(authFile2)

	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"rate_limited"}`))),
					Header:     http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-codex",
		AuthFiles:          []string{authFile1, authFile2},
		RequestCompression: "none",
	}

	statusCode, respBody, _, err := ForwardToCodex(client, upstream, []byte(`{"input":"hello"}`), false)
	if err != nil {
		t.Fatalf("unexpected error: %v (should return status/body, not error)", err)
	}
	if statusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", statusCode)
	}
	if respBody == nil {
		t.Fatal("expected response body")
	}
}

func TestForwardToCodex_AuthTokenError_SkipsToNext(t *testing.T) {
	tmpDir := t.TempDir()
	authFile1 := filepath.Join(tmpDir, "auth1.json")
	authFile2 := filepath.Join(tmpDir, "auth2.json")

	s2 := &CodexTokenStorage{
		AccessToken:  "tok2",
		AccountID:    "acct2",
		RefreshToken: "ref2",
		Email:        "u2@test.com",
	}
	d2, _ := json.Marshal(s2)
	os.WriteFile(authFile2, d2, 0644)

	codexAuthManager.StoreEntry(authFile2, s2, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer codexAuthManager.DeleteEntry(authFile2)

	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				sse := `data: {"type":"response.completed","response":{"id":"r2","model":"codex-mini"}}` + "\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(sse))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-codex",
		AuthFiles:          []string{authFile1, authFile2},
		RequestCompression: "none",
	}

	statusCode, _, _, err := ForwardToCodex(client, upstream, []byte(`{"input":"hello"}`), false)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", statusCode)
	}
}

func TestForwardToCodexStream_AuthFileFailover(t *testing.T) {
	tmpDir := t.TempDir()
	authFile1 := filepath.Join(tmpDir, "auth1.json")
	authFile2 := filepath.Join(tmpDir, "auth2.json")

	s1 := &CodexTokenStorage{AccessToken: "tok1", RefreshToken: "ref1", Email: "u1@test.com"}
	s2 := &CodexTokenStorage{AccessToken: "tok2", RefreshToken: "ref2", Email: "u2@test.com"}
	d1, _ := json.Marshal(s1)
	d2, _ := json.Marshal(s2)
	os.WriteFile(authFile1, d1, 0644)
	os.WriteFile(authFile2, d2, 0644)

	codexAuthManager.StoreEntry(authFile1, s1, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	codexAuthManager.StoreEntry(authFile2, s2, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer codexAuthManager.DeleteEntry(authFile1)
	defer codexAuthManager.DeleteEntry(authFile2)

	var callCount atomic.Int32
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				n := callCount.Add(1)
				if n == 1 {
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"rate_limited"}`))),
						Header:     http.Header{"Content-Type": {"application/json"}},
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte("data: ok\n"))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-codex",
		AuthFiles:          []string{authFile1, authFile2},
		RequestCompression: "none",
	}

	resp, err := ForwardToCodexStream(client, upstream, []byte(`{"input":"hello"}`), context.Background())
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestForwardToCodexStream_ClosesPriorErrorResponseOnSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	authFile1 := filepath.Join(tmpDir, "auth1.json")
	authFile2 := filepath.Join(tmpDir, "auth2.json")

	s1 := &CodexTokenStorage{AccessToken: "tok1", RefreshToken: "ref1", Email: "u1@test.com"}
	s2 := &CodexTokenStorage{AccessToken: "tok2", RefreshToken: "ref2", Email: "u2@test.com"}
	d1, _ := json.Marshal(s1)
	d2, _ := json.Marshal(s2)
	os.WriteFile(authFile1, d1, 0644)
	os.WriteFile(authFile2, d2, 0644)

	codexAuthManager.StoreEntry(authFile1, s1, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	codexAuthManager.StoreEntry(authFile2, s2, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer codexAuthManager.DeleteEntry(authFile1)
	defer codexAuthManager.DeleteEntry(authFile2)

	var firstBodyClosed atomic.Bool
	var callCount atomic.Int32
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				n := callCount.Add(1)
				if n == 1 {
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Body: &trackedReadCloser{
							Reader: bytes.NewReader([]byte(`{"error":"rate_limited"}`)),
							closed: &firstBodyClosed,
						},
						Header: http.Header{"Content-Type": {"application/json"}},
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte("data: ok\n"))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-codex",
		AuthFiles:          []string{authFile1, authFile2},
		RequestCompression: "none",
	}

	resp, err := ForwardToCodexStream(client, upstream, []byte(`{"input":"hello"}`), context.Background())
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !firstBodyClosed.Load() {
		t.Fatal("expected first failed response body to be closed before returning success")
	}
}

func TestForwardToCodexStream_AllFail_ReturnsLastResponse(t *testing.T) {
	tmpDir := t.TempDir()
	authFile1 := filepath.Join(tmpDir, "auth1.json")

	s1 := &CodexTokenStorage{AccessToken: "tok1", RefreshToken: "ref1"}
	d1, _ := json.Marshal(s1)
	os.WriteFile(authFile1, d1, 0644)

	codexAuthManager.StoreEntry(authFile1, s1, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer codexAuthManager.DeleteEntry(authFile1)

	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"overloaded"}`))),
					Header:     http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-codex",
		AuthFiles:          []string{authFile1},
		RequestCompression: "none",
	}

	resp, err := ForwardToCodexStream(client, upstream, []byte(`{"input":"hello"}`), context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v (should return *http.Response)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

type trackedReadCloser struct {
	Reader *bytes.Reader
	closed *atomic.Bool
}

func (t *trackedReadCloser) Read(p []byte) (int, error) {
	return t.Reader.Read(p)
}

func (t *trackedReadCloser) Close() error {
	t.closed.Store(true)
	return nil
}

func TestForwardToGeminiCLI_AuthFileFailover_ProjectIDInjection(t *testing.T) {
	tmpDir := t.TempDir()
	authFile1 := filepath.Join(tmpDir, "auth1.json")
	authFile2 := filepath.Join(tmpDir, "auth2.json")

	expiry := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	s1 := &GeminiCLITokenStorage{
		Token: map[string]any{
			"access_token":  "tok1",
			"refresh_token": "ref1",
			"expiry":        expiry,
		},
		ProjectID: "proj-1",
		Email:     "u1@test.com",
	}
	s2 := &GeminiCLITokenStorage{
		Token: map[string]any{
			"access_token":  "tok2",
			"refresh_token": "ref2",
			"expiry":        expiry,
		},
		ProjectID: "proj-2",
		Email:     "u2@test.com",
	}
	d1, _ := json.Marshal(s1)
	d2, _ := json.Marshal(s2)
	os.WriteFile(authFile1, d1, 0644)
	os.WriteFile(authFile2, d2, 0644)

	geminiCLIAuthManager.StoreEntry(authFile1, s1, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	geminiCLIAuthManager.StoreEntry(authFile2, s2, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer geminiCLIAuthManager.DeleteEntry(authFile1)
	defer geminiCLIAuthManager.DeleteEntry(authFile2)

	var capturedProject string
	var callCount atomic.Int32
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				n := callCount.Add(1)
				bodyBytes, _ := io.ReadAll(req.Body)
				var body map[string]any
				json.Unmarshal(bodyBytes, &body)

				if n == 1 {
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"rate_limited"}`))),
						Header:     http.Header{"Content-Type": {"application/json"}},
					}, nil
				}
				capturedProject, _ = body["project"].(string)
				sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(sse))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-geminicli",
		AuthFiles:          []string{authFile1, authFile2},
		RequestCompression: "none",
	}

	statusCode, _, _, err := ForwardToGeminiCLI(client, upstream, []byte(`{"input":"hello","model":"gemini-3-pro-preview"}`), false)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", statusCode)
	}
	if capturedProject != "proj-2" {
		t.Fatalf("project = %q, want %q (from second auth file)", capturedProject, "proj-2")
	}
}

func TestForwardToAntigravity_AuthFileFailover_ProjectIDAndRequestID(t *testing.T) {
	tmpDir := t.TempDir()
	authFile1 := filepath.Join(tmpDir, "auth1.json")
	authFile2 := filepath.Join(tmpDir, "auth2.json")

	s1 := &AntigravityTokenStorage{
		AccessToken:  "tok1",
		RefreshToken: "ref1",
		ProjectID:    "proj-1",
		Email:        "u1@test.com",
		Expired:      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
	}
	s2 := &AntigravityTokenStorage{
		AccessToken:  "tok2",
		RefreshToken: "ref2",
		ProjectID:    "proj-2",
		Email:        "u2@test.com",
		Expired:      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
	}
	d1, _ := json.Marshal(s1)
	d2, _ := json.Marshal(s2)
	os.WriteFile(authFile1, d1, 0644)
	os.WriteFile(authFile2, d2, 0644)

	antigravityAuthManager.StoreEntry(authFile1, s1, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	antigravityAuthManager.StoreEntry(authFile2, s2, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer antigravityAuthManager.DeleteEntry(authFile1)
	defer antigravityAuthManager.DeleteEntry(authFile2)

	var capturedProject string
	var capturedRequestIDs []string
	var callCount atomic.Int32
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				n := callCount.Add(1)
				bodyBytes, _ := io.ReadAll(req.Body)
				var body map[string]any
				json.Unmarshal(bodyBytes, &body)

				if rid, ok := body["requestId"].(string); ok {
					capturedRequestIDs = append(capturedRequestIDs, rid)
				}

				if n == 1 {
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"rate_limited"}`))),
						Header:     http.Header{"Content-Type": {"application/json"}},
					}, nil
				}
				capturedProject, _ = body["project"].(string)
				sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(sse))),
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-antigravity",
		AuthFiles:          []string{authFile1, authFile2},
		RequestCompression: "none",
	}

	statusCode, _, _, err := ForwardToAntigravity(client, upstream, []byte(`{"input":"hello","model":"gemini-3-pro-high"}`), false)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", statusCode)
	}
	if capturedProject != "proj-2" {
		t.Fatalf("project = %q, want %q", capturedProject, "proj-2")
	}
	if len(capturedRequestIDs) != 2 {
		t.Fatalf("expected 2 request IDs, got %d", len(capturedRequestIDs))
	}
	if capturedRequestIDs[0] == capturedRequestIDs[1] {
		t.Fatal("requestId should be different for each attempt")
	}
}

func TestForwardToClaudeCode_AuthFileFailover(t *testing.T) {
	tmpDir := t.TempDir()
	authFile1 := filepath.Join(tmpDir, "auth1.json")
	authFile2 := filepath.Join(tmpDir, "auth2.json")

	s1 := &ClaudeCodeTokenStorage{
		AccessToken:  "tok1",
		RefreshToken: "ref1",
		Email:        "u1@test.com",
	}
	s2 := &ClaudeCodeTokenStorage{
		AccessToken:  "tok2",
		RefreshToken: "ref2",
		Email:        "u2@test.com",
	}
	d1, _ := json.Marshal(s1)
	d2, _ := json.Marshal(s2)
	os.WriteFile(authFile1, d1, 0644)
	os.WriteFile(authFile2, d2, 0644)

	claudeCodeAuthManager.StoreEntry(authFile1, s1, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	claudeCodeAuthManager.StoreEntry(authFile2, s2, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer claudeCodeAuthManager.DeleteEntry(authFile1)
	defer claudeCodeAuthManager.DeleteEntry(authFile2)

	var callCount atomic.Int32
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				n := callCount.Add(1)
				if n == 1 {
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"rate_limited"}`))),
						Header:     http.Header{"Content-Type": {"application/json"}},
					}, nil
				}
				body := `{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"model":"claude-sonnet-4","stop_reason":"end_turn"}`
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(body))),
					Header:     http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:               "test-claudecode",
		AuthFiles:          []string{authFile1, authFile2},
		RequestCompression: "none",
	}

	statusCode, respBody, _, err := ForwardToClaudeCode(client, upstream,
		[]byte(fmt.Sprintf(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)),
		false)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", statusCode)
	}
	if callCount.Load() != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d", callCount.Load())
	}
	var resp map[string]any
	json.Unmarshal(respBody, &resp)
	if resp["id"] != "msg-1" {
		t.Fatalf("response id = %q, want %q", resp["id"], "msg-1")
	}
}
