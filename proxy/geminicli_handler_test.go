package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// mockRoundTripper intercepts HTTP requests for testing.
type mockRoundTripper struct {
	handler func(req *http.Request) (*http.Response, error)
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.handler(req)
}

func TestGeminiCLIAuth_ProjectIDRetryAfterFailure(t *testing.T) {
	var discoveryCallCount atomic.Int32

	// Create a temp auth file with a valid token but no project_id
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "auth.json")
	expiry := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	storage := &GeminiCLITokenStorage{
		Token: map[string]any{
			"access_token":  "test-token",
			"refresh_token": "test-refresh",
			"expiry":        expiry,
		},
		Email: "test@example.com",
		// ProjectID intentionally empty
	}
	data, _ := json.Marshal(storage)
	if err := os.WriteFile(authFile, data, 0644); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	ga := NewGeminiCLIAuth(authFile)
	// Override the HTTP client with a mock transport
	ga.client = &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				count := discoveryCallCount.Add(1)
				if count == 1 {
					// First discovery call fails
					return &http.Response{
						StatusCode: http.StatusServiceUnavailable,
						Body:       io.NopCloser(bytes.NewReader([]byte("service unavailable"))),
						Header:     make(http.Header),
					}, nil
				}
				// Subsequent discovery calls succeed
				body, _ := json.Marshal(map[string]any{
					"cloudaicompanionProject": "project-123",
				})
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
	}

	// First call: project discovery fails, should NOT cache storage in-memory
	token1, stor1, err := ga.GetAccessToken()
	if err != nil {
		t.Fatalf("first GetAccessToken() error = %v", err)
	}
	if token1 != "test-token" {
		t.Fatalf("token = %q, want %q", token1, "test-token")
	}
	if stor1.ProjectID != "" {
		t.Fatalf("first call: ProjectID = %q, want empty", stor1.ProjectID)
	}
	// Verify storage was NOT cached (ga.storage should remain nil)
	ga.mu.RLock()
	cachedAfterFirst := ga.storage
	ga.mu.RUnlock()
	if cachedAfterFirst != nil {
		t.Fatal("storage should NOT be cached after failed project_id discovery")
	}
	if c := discoveryCallCount.Load(); c != 1 {
		t.Fatalf("discovery calls = %d, want 1", c)
	}

	// Second call: should retry discovery (reload from file), succeed this time
	token2, stor2, err := ga.GetAccessToken()
	if err != nil {
		t.Fatalf("second GetAccessToken() error = %v", err)
	}
	if token2 != "test-token" {
		t.Fatalf("token = %q, want %q", token2, "test-token")
	}
	if stor2.ProjectID != "project-123" {
		t.Fatalf("second call: ProjectID = %q, want %q", stor2.ProjectID, "project-123")
	}
	if c := discoveryCallCount.Load(); c != 2 {
		t.Fatalf("discovery calls = %d, want 2", c)
	}

	// Third call: should hit the read-lock fast path (cached), no more discovery calls
	_, stor3, err := ga.GetAccessToken()
	if err != nil {
		t.Fatalf("third GetAccessToken() error = %v", err)
	}
	if stor3.ProjectID != "project-123" {
		t.Fatalf("third call: ProjectID = %q, want %q", stor3.ProjectID, "project-123")
	}
	if c := discoveryCallCount.Load(); c != 2 {
		t.Fatalf("discovery calls = %d, want 2 (should use cache)", c)
	}
}
