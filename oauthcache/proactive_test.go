package oauthcache

import (
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetToken_GetExpirySkipsRefreshWhenValid(t *testing.T) {
	var refreshCount atomic.Int32

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		refreshCount.Add(1)
		s.AccessToken = "refreshed"
		return 1 * time.Hour, nil
	})
	cfg.Load = func(path string) (*testStorage, error) {
		return &testStorage{AccessToken: "disk-token", RefreshToken: "ref"}, nil
	}
	cfg.GetExpiry = func(s *testStorage) time.Time {
		return time.Now().Add(2 * time.Hour)
	}
	mgr := NewManager(cfg)

	tok, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken error: %v", err)
	}
	if tok != "disk-token" {
		t.Fatalf("token = %q, want %q (should reuse disk token)", tok, "disk-token")
	}
	if c := refreshCount.Load(); c != 0 {
		t.Fatalf("refresh calls = %d, want 0 (GetExpiry should skip refresh)", c)
	}
}

func TestGetToken_GetExpiryFallsThroughWhenExpiringSoon(t *testing.T) {
	var refreshCount atomic.Int32

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		refreshCount.Add(1)
		s.AccessToken = "refreshed"
		return 1 * time.Hour, nil
	})
	cfg.Load = func(path string) (*testStorage, error) {
		return &testStorage{AccessToken: "disk-token", RefreshToken: "ref"}, nil
	}
	cfg.GetExpiry = func(s *testStorage) time.Time {
		return time.Now().Add(30 * time.Second) // within 60s window
	}
	mgr := NewManager(cfg)

	tok, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken error: %v", err)
	}
	if tok != "refreshed" {
		t.Fatalf("token = %q, want %q (should have refreshed)", tok, "refreshed")
	}
	if c := refreshCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1", c)
	}
}

func TestGetToken_GetExpiryFallsThroughOnZeroTime(t *testing.T) {
	var refreshCount atomic.Int32

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		refreshCount.Add(1)
		s.AccessToken = "refreshed"
		return 1 * time.Hour, nil
	})
	cfg.Load = func(path string) (*testStorage, error) {
		return &testStorage{AccessToken: "disk-token", RefreshToken: "ref"}, nil
	}
	cfg.GetExpiry = func(s *testStorage) time.Time {
		return time.Time{} // zero = not ready
	}
	mgr := NewManager(cfg)

	tok, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken error: %v", err)
	}
	if tok != "refreshed" {
		t.Fatalf("token = %q, want %q", tok, "refreshed")
	}
	if c := refreshCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1", c)
	}
}

func TestGetToken_GetExpiryFallsThroughOnEmptyAccessToken(t *testing.T) {
	var refreshCount atomic.Int32

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		refreshCount.Add(1)
		s.AccessToken = "refreshed"
		return 1 * time.Hour, nil
	})
	cfg.Load = func(path string) (*testStorage, error) {
		return &testStorage{AccessToken: "", RefreshToken: "ref"}, nil
	}
	cfg.GetExpiry = func(s *testStorage) time.Time {
		return time.Now().Add(2 * time.Hour)
	}
	mgr := NewManager(cfg)

	tok, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken error: %v", err)
	}
	if tok != "refreshed" {
		t.Fatalf("token = %q, want %q (empty access_token should trigger refresh)", tok, "refreshed")
	}
	if c := refreshCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1", c)
	}
}

func TestGetToken_GetExpiryNilDoesNotShortCircuit(t *testing.T) {
	var refreshCount atomic.Int32

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		refreshCount.Add(1)
		s.AccessToken = "refreshed"
		return 1 * time.Hour, nil
	})
	cfg.Load = func(path string) (*testStorage, error) {
		return &testStorage{AccessToken: "disk-token", RefreshToken: "ref"}, nil
	}
	// GetExpiry is nil (default)
	mgr := NewManager(cfg)

	tok, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken error: %v", err)
	}
	if tok != "refreshed" {
		t.Fatalf("token = %q, want %q", tok, "refreshed")
	}
	if c := refreshCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1", c)
	}
}

func TestRefreshAll_SkipsDisabledFiles(t *testing.T) {
	var refreshCount atomic.Int32

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		refreshCount.Add(1)
		if authFile == "disabled.json" {
			return 0, &ErrAuthDisabled{AuthFile: authFile, Reason: "HTTP 401"}
		}
		s.AccessToken = "good"
		return 1 * time.Hour, nil
	})
	cfg.Disable = func(path string, s *testStorage) error { return nil }
	mgr := NewManager(cfg)

	for _, f := range []string{"disabled.json", "good.json"} {
		mgr.StoreEntry(f, &testStorage{RefreshToken: "ref"}, time.Now().Add(-1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	}

	// First call: disabled.json gets disabled, good.json refreshed
	mgr.RefreshAll([]string{"disabled.json", "good.json"}, 30*time.Second)

	// Second call: disabled.json should be skipped silently
	before := refreshCount.Load()
	mgr.RefreshAll([]string{"disabled.json", "good.json"}, 30*time.Second)
	after := refreshCount.Load()

	// Only good.json should have been checked (cache hit, no refresh needed)
	if after != before {
		t.Fatalf("refresh calls changed from %d to %d (disabled file should be skipped)", before, after)
	}
}

func TestRefreshAll_HandlesAllErrors(t *testing.T) {
	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		return 0, fmt.Errorf("network error")
	})
	mgr := NewManager(cfg)

	mgr.StoreEntry("a.json", &testStorage{RefreshToken: "ref"}, time.Now().Add(-1*time.Hour), &http.Client{Timeout: 30 * time.Second})

	// Should not panic
	mgr.RefreshAll([]string{"a.json"}, 30*time.Second)
}

func TestRefreshAll_EmptyFiles(t *testing.T) {
	cfg := testConfig(nil)
	mgr := NewManager(cfg)

	// Should not panic on empty file list
	mgr.RefreshAll(nil, 30*time.Second)
	mgr.RefreshAll([]string{}, 30*time.Second)
}

func TestGetToken_GetExpiryPastTimeTriggersRefresh(t *testing.T) {
	var refreshCount atomic.Int32

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		refreshCount.Add(1)
		s.AccessToken = "refreshed"
		return 1 * time.Hour, nil
	})
	cfg.Load = func(path string) (*testStorage, error) {
		return &testStorage{AccessToken: "disk-token", RefreshToken: "ref"}, nil
	}
	cfg.GetExpiry = func(s *testStorage) time.Time {
		return time.Now().Add(-1 * time.Hour) // already expired
	}
	mgr := NewManager(cfg)

	tok, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken error: %v", err)
	}
	if tok != "refreshed" {
		t.Fatalf("token = %q, want %q", tok, "refreshed")
	}
	if c := refreshCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1 (past expiry should trigger refresh)", c)
	}
}

func TestRefresherInterface(t *testing.T) {
	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		s.AccessToken = "ok"
		return 1 * time.Hour, nil
	})
	mgr := NewManager(cfg)
	mgr.StoreEntry("a.json", &testStorage{RefreshToken: "ref"}, time.Now().Add(-1*time.Hour), &http.Client{Timeout: 30 * time.Second})

	// Verify Manager satisfies Refresher interface
	var r Refresher = mgr
	r.RefreshAll([]string{"a.json"}, 30*time.Second)

	tok, _, err := mgr.GetToken("a.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken error: %v", err)
	}
	if tok != "ok" {
		t.Fatalf("token = %q, want %q", tok, "ok")
	}
}

func TestRefreshAll_DisabledErrorNotLogged(t *testing.T) {
	cfg := testConfig(nil)
	cfg.Load = func(path string) (*testStorage, error) {
		return nil, &ErrAuthDisabled{AuthFile: path, Reason: "marked disabled"}
	}
	mgr := NewManager(cfg)

	// First call to mark it disabled via GetToken
	_, _, err := mgr.GetToken("disabled.json", 30*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	var de *ErrAuthDisabled
	if !errors.As(err, &de) {
		t.Fatalf("expected ErrAuthDisabled, got %T", err)
	}

	// RefreshAll should silently skip without panic
	mgr.RefreshAll([]string{"disabled.json"}, 30*time.Second)
}
