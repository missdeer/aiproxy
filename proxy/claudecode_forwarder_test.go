package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── claudeCodeLoadStorage / claudeCodeSaveStorage tests ────────────────

func TestClaudeCodeLoadAndSaveStorage(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	original := &ClaudeCodeTokenStorage{
		AccessToken:  "access-tok",
		RefreshToken: "refresh-tok",
		Email:        "test@claude.com",
		Expire:       "2026-12-31T00:00:00Z",
		LastRefresh:  "2026-01-01T00:00:00Z",
		Type:         "claudecode",
	}

	if err := claudeCodeSaveStorage(authFile, original); err != nil {
		t.Fatalf("claudeCodeSaveStorage() error = %v", err)
	}

	loaded, err := claudeCodeLoadStorage(authFile)
	if err != nil {
		t.Fatalf("claudeCodeLoadStorage() error = %v", err)
	}

	if loaded.AccessToken != original.AccessToken {
		t.Fatalf("AccessToken = %q, want %q", loaded.AccessToken, original.AccessToken)
	}
	if loaded.RefreshToken != original.RefreshToken {
		t.Fatalf("RefreshToken = %q, want %q", loaded.RefreshToken, original.RefreshToken)
	}
	if loaded.Email != original.Email {
		t.Fatalf("Email = %q, want %q", loaded.Email, original.Email)
	}
	if loaded.Type != original.Type {
		t.Fatalf("Type = %q, want %q", loaded.Type, original.Type)
	}
}

func TestClaudeCodeLoadStorage_FileNotFound(t *testing.T) {
	_, err := claudeCodeLoadStorage("/nonexistent/auth.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestClaudeCodeLoadStorage_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "bad.json")
	os.WriteFile(authFile, []byte("not json"), 0644)

	_, err := claudeCodeLoadStorage(authFile)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ── claudeCodeMapStainless tests ───────────────────────────────────────

func TestClaudeCodeMapStainlessOS(t *testing.T) {
	result := claudeCodeMapStainlessOS()
	switch runtime.GOOS {
	case "darwin":
		if result != "MacOS" {
			t.Fatalf("got %q, want %q", result, "MacOS")
		}
	case "windows":
		if result != "Windows" {
			t.Fatalf("got %q, want %q", result, "Windows")
		}
	case "linux":
		if result != "Linux" {
			t.Fatalf("got %q, want %q", result, "Linux")
		}
	default:
		if !strings.HasPrefix(result, "Other::") {
			t.Fatalf("got %q, want prefix %q", result, "Other::")
		}
	}
}

func TestClaudeCodeMapStainlessArch(t *testing.T) {
	result := claudeCodeMapStainlessArch()
	switch runtime.GOARCH {
	case "amd64":
		if result != "x64" {
			t.Fatalf("got %q, want %q", result, "x64")
		}
	case "arm64":
		if result != "arm64" {
			t.Fatalf("got %q, want %q", result, "arm64")
		}
	case "386":
		if result != "x86" {
			t.Fatalf("got %q, want %q", result, "x86")
		}
	default:
		if !strings.HasPrefix(result, "other::") {
			t.Fatalf("got %q, want prefix %q", result, "other::")
		}
	}
}

// ── claudeCodeSSEToJSON tests ──────────────────────────────────────────

func TestClaudeCodeSSEToJSON(t *testing.T) {
	t.Run("assembles full message from SSE", func(t *testing.T) {
		sse := "event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg-cc","model":"claude-sonnet-4","usage":{"input_tokens":20}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" from Claude Code"}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"

		result, err := claudeCodeSSEToJSON([]byte(sse))
		if err != nil {
			t.Fatalf("error = %v", err)
		}

		var resp map[string]any
		json.Unmarshal(result, &resp)

		if resp["id"] != "msg-cc" {
			t.Fatalf("id = %q, want %q", resp["id"], "msg-cc")
		}
		if resp["model"] != "claude-sonnet-4" {
			t.Fatalf("model = %q, want %q", resp["model"], "claude-sonnet-4")
		}
		if resp["stop_reason"] != "end_turn" {
			t.Fatalf("stop_reason = %q, want %q", resp["stop_reason"], "end_turn")
		}

		content := resp["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("content length = %d, want 1", len(content))
		}
		block := content[0].(map[string]any)
		if block["text"] != "Hello from Claude Code" {
			t.Fatalf("text = %q, want %q", block["text"], "Hello from Claude Code")
		}
	})

	t.Run("handles thinking deltas", func(t *testing.T) {
		sse := "event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Step 1"}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":", Step 2"}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"

		result, err := claudeCodeSSEToJSON([]byte(sse))
		if err != nil {
			t.Fatalf("error = %v", err)
		}

		var resp map[string]any
		json.Unmarshal(result, &resp)
		content := resp["content"].([]any)
		block := content[0].(map[string]any)
		if block["thinking"] != "Step 1, Step 2" {
			t.Fatalf("thinking = %q, want %q", block["thinking"], "Step 1, Step 2")
		}
	})

	t.Run("error event returns error", func(t *testing.T) {
		sse := "event: error\n" +
			`data: {"type":"error","error":{"message":"overloaded"}}` + "\n\n"

		_, err := claudeCodeSSEToJSON([]byte(sse))
		if err == nil {
			t.Fatal("expected error for SSE error event")
		}
		if !strings.Contains(err.Error(), "overloaded") {
			t.Fatalf("error = %q, want to contain 'overloaded'", err.Error())
		}
	})

	t.Run("empty SSE data", func(t *testing.T) {
		result, err := claudeCodeSSEToJSON([]byte(""))
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		var resp map[string]any
		json.Unmarshal(result, &resp)
		if resp["type"] != "message" {
			t.Fatalf("type = %q, want %q", resp["type"], "message")
		}
		if resp["role"] != "assistant" {
			t.Fatalf("role = %q, want %q", resp["role"], "assistant")
		}
	})
}

// ── ClaudeCodeAuth.GetAccessToken tests ────────────────────────────────

func TestClaudeCodeAuth_GetAccessToken_RefreshesAndCaches(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	initial := &ClaudeCodeTokenStorage{
		RefreshToken: "my-refresh-token",
		Email:        "old@claude.com",
	}
	data, _ := json.Marshal(initial)
	os.WriteFile(authFile, data, 0644)

	var refreshCallCount atomic.Int32

	ca := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				refreshCallCount.Add(1)
				tokResp := ClaudeCodeTokenResponse{
					AccessToken:  "new-access-token",
					RefreshToken: "new-refresh-token",
					TokenType:    "Bearer",
					ExpiresIn:    3600,
				}
				tokResp.Account.EmailAddress = "new@claude.com"
				body, _ := json.Marshal(tokResp)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
	}

	claudeCodeAuthManager.StoreEntry(authFile, initial, time.Time{}, ca)
	defer claudeCodeAuthManager.DeleteEntry(authFile)

	// First call triggers refresh
	token1, stor1, err := claudeCodeAuthManager.GetToken(authFile, 30*time.Second)
	if err != nil {
		t.Fatalf("first GetAccessToken() error = %v", err)
	}
	if token1 != "new-access-token" {
		t.Fatalf("token = %q, want %q", token1, "new-access-token")
	}
	if stor1.Email != "new@claude.com" {
		t.Fatalf("email = %q, want %q", stor1.Email, "new@claude.com")
	}
	if c := refreshCallCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1", c)
	}

	// Second call uses cache
	token2, _, err := claudeCodeAuthManager.GetToken(authFile, 30*time.Second)
	if err != nil {
		t.Fatalf("second GetAccessToken() error = %v", err)
	}
	if token2 != "new-access-token" {
		t.Fatalf("cached token = %q, want %q", token2, "new-access-token")
	}
	if c := refreshCallCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1 (should use cache)", c)
	}
}

func TestClaudeCodeAuth_GetAccessToken_EmptyRefreshToken(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	s := &ClaudeCodeTokenStorage{Email: "test@example.com"}
	data, _ := json.Marshal(s)
	os.WriteFile(authFile, data, 0644)

	claudeCodeAuthManager.StoreEntry(authFile, s, time.Time{}, &http.Client{Timeout: 30 * time.Second})
	defer claudeCodeAuthManager.DeleteEntry(authFile)
	_, _, err := claudeCodeAuthManager.GetToken(authFile, 30*time.Second)
	if err == nil {
		t.Fatal("expected error for empty refresh_token")
	}
}

func TestClaudeCodeAuth_GetAccessToken_FileNotFound(t *testing.T) {
	_, _, err := claudeCodeAuthManager.GetToken("/nonexistent/auth.json", 30*time.Second)
	if err == nil {
		t.Fatal("expected error for missing auth file")
	}
}

func TestClaudeCodeAuth_GetAccessToken_RefreshFails(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")

	s := &ClaudeCodeTokenStorage{RefreshToken: "my-refresh"}
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

	claudeCodeAuthManager.StoreEntry(authFile, s, time.Time{}, ca)
	defer claudeCodeAuthManager.DeleteEntry(authFile)

	_, _, err := claudeCodeAuthManager.GetToken(authFile, 30*time.Second)
	if err == nil {
		t.Fatal("expected error when refresh fails")
	}
}
