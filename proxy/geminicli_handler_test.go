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

	mockClient := &http.Client{
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

	geminiCLIAuthManager.StoreEntry(authFile, storage, time.Time{}, mockClient)
	defer geminiCLIAuthManager.DeleteEntry(authFile)

	// First call: project discovery fails, should not cache (expiresIn=0 returned)
	token1, stor1, err := geminiCLIAuthManager.GetToken(authFile, 30*time.Second)
	if err != nil {
		t.Fatalf("first GetToken() error = %v", err)
	}
	if token1 != "test-token" {
		t.Fatalf("token = %q, want %q", token1, "test-token")
	}
	if stor1.ProjectID != "" {
		t.Fatalf("first call: ProjectID = %q, want empty", stor1.ProjectID)
	}

	if c := discoveryCallCount.Load(); c != 1 {
		t.Fatalf("discovery calls = %d, want 1", c)
	}

	// Second call: should retry discovery, succeed this time
	token2, stor2, err := geminiCLIAuthManager.GetToken(authFile, 30*time.Second)
	if err != nil {
		t.Fatalf("second GetToken() error = %v", err)
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

	// Third call: should hit cache, no more discovery calls
	_, stor3, err := geminiCLIAuthManager.GetToken(authFile, 30*time.Second)
	if err != nil {
		t.Fatalf("third GetToken() error = %v", err)
	}
	if stor3.ProjectID != "project-123" {
		t.Fatalf("third call: ProjectID = %q, want %q", stor3.ProjectID, "project-123")
	}
	if c := discoveryCallCount.Load(); c != 2 {
		t.Fatalf("discovery calls = %d, want 2 (should use cache)", c)
	}
}
