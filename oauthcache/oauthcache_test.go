package oauthcache

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testStorage is a simple storage type for testing.
type testStorage struct {
	AccessToken  string
	RefreshToken string
	Email        string
}

// testConfig returns a basic Config for testing.
func testConfig(refreshFn func(client *http.Client, s *testStorage, authFile string) (time.Duration, error)) Config[testStorage] {
	return Config[testStorage]{
		Label: "[TEST]",
		Load: func(path string) (*testStorage, error) {
			return nil, fmt.Errorf("file not found: %s", path)
		},
		Save: func(path string, s *testStorage) error {
			return nil
		},
		GetAccessToken: func(s *testStorage) string {
			return s.AccessToken
		},
		Copy: func(s *testStorage) testStorage {
			return *s
		},
		Refresh: refreshFn,
	}
}

func TestGetToken_RefreshesAndCaches(t *testing.T) {
	var callCount atomic.Int32

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		callCount.Add(1)
		s.AccessToken = "refreshed-token"
		s.Email = "user@test.com"
		return 1 * time.Hour, nil
	})
	mgr := NewManager(cfg)

	// Pre-populate with expired entry
	mgr.StoreEntry("auth.json", &testStorage{
		RefreshToken: "refresh-tok",
	}, time.Time{}, &http.Client{})

	// First call triggers refresh
	tok, stor, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("first GetToken error: %v", err)
	}
	if tok != "refreshed-token" {
		t.Fatalf("token = %q, want %q", tok, "refreshed-token")
	}
	if stor.Email != "user@test.com" {
		t.Fatalf("email = %q, want %q", stor.Email, "user@test.com")
	}
	if c := callCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1", c)
	}

	// Second call should use cache (no additional refresh)
	tok2, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("second GetToken error: %v", err)
	}
	if tok2 != "refreshed-token" {
		t.Fatalf("cached token = %q, want %q", tok2, "refreshed-token")
	}
	if c := callCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1 (should use cache)", c)
	}
}

func TestGetToken_FileNotFound(t *testing.T) {
	cfg := testConfig(nil)
	mgr := NewManager(cfg)

	// No pre-stored entry, Load returns error
	_, _, err := mgr.GetToken("/nonexistent/auth.json", 30*time.Second)
	if err == nil {
		t.Fatal("expected error for missing auth file")
	}
}

func TestGetToken_EmptyRefreshToken(t *testing.T) {
	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		if s.RefreshToken == "" {
			return 0, fmt.Errorf("refresh_token is empty in %s", authFile)
		}
		return 0, nil
	})
	mgr := NewManager(cfg)

	// Pre-populate with no refresh token
	mgr.StoreEntry("auth.json", &testStorage{
		AccessToken: "old-access",
		// RefreshToken intentionally empty
	}, time.Time{}, &http.Client{})

	_, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err == nil {
		t.Fatal("expected error for empty refresh_token")
	}
}

func TestGetToken_RefreshError(t *testing.T) {
	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		return 0, fmt.Errorf("refresh failed")
	})
	mgr := NewManager(cfg)

	mgr.StoreEntry("auth.json", &testStorage{
		RefreshToken: "refresh-tok",
	}, time.Time{}, &http.Client{})

	_, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err == nil {
		t.Fatal("expected error when refresh fails")
	}
}

func TestGetToken_Singleflight(t *testing.T) {
	var callCount atomic.Int32
	var wg sync.WaitGroup

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		callCount.Add(1)
		time.Sleep(50 * time.Millisecond) // simulate slow refresh
		s.AccessToken = "singleflight-token"
		return 1 * time.Hour, nil
	})
	mgr := NewManager(cfg)

	mgr.StoreEntry("auth.json", &testStorage{
		RefreshToken: "refresh-tok",
	}, time.Time{}, &http.Client{})

	// Launch 10 concurrent GetToken calls
	const n = 10
	tokens := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tok, _, err := mgr.GetToken("auth.json", 30*time.Second)
			tokens[idx] = tok
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// All should succeed with same token
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d error: %v", i, errs[i])
		}
		if tokens[i] != "singleflight-token" {
			t.Fatalf("goroutine %d token = %q, want %q", i, tokens[i], "singleflight-token")
		}
	}

	// Singleflight should have merged calls: only 1 refresh
	if c := callCount.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1 (singleflight should merge)", c)
	}
}

func TestGetToken_AsyncSave(t *testing.T) {
	var saved atomic.Bool

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		s.AccessToken = "saved-token"
		return 1 * time.Hour, nil
	})
	cfg.Save = func(path string, s *testStorage) error {
		saved.Store(true)
		return nil
	}
	mgr := NewManager(cfg)

	mgr.StoreEntry("auth.json", &testStorage{
		RefreshToken: "refresh-tok",
	}, time.Time{}, &http.Client{})

	_, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken error: %v", err)
	}

	// Wait for async goroutine
	time.Sleep(100 * time.Millisecond)

	if !saved.Load() {
		t.Fatal("Save was not called (async save failed)")
	}
}

func TestGetToken_CopyIsolation(t *testing.T) {
	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		s.AccessToken = "original"
		return 1 * time.Hour, nil
	})
	mgr := NewManager(cfg)

	mgr.StoreEntry("auth.json", &testStorage{
		RefreshToken: "refresh-tok",
	}, time.Time{}, &http.Client{})

	_, stor1, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken error: %v", err)
	}

	// Mutate the returned storage
	stor1.Email = "mutated"

	// Get again, should NOT see the mutation
	_, stor2, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("second GetToken error: %v", err)
	}
	if stor2.Email == "mutated" {
		t.Fatal("Copy isolation failed: mutation leaked through")
	}
}

func TestStoreEntry_DeleteEntry(t *testing.T) {
	cfg := testConfig(nil)
	mgr := NewManager(cfg)

	s := &testStorage{
		AccessToken:  "pre-cached",
		RefreshToken: "refresh",
	}
	mgr.StoreEntry("auth.json", s, time.Now().Add(1*time.Hour), &http.Client{})

	// Should return cached token
	tok, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken error: %v", err)
	}
	if tok != "pre-cached" {
		t.Fatalf("token = %q, want %q", tok, "pre-cached")
	}

	// Delete the entry
	mgr.DeleteEntry("auth.json")

	// Should now fail (Load returns file not found)
	_, _, err = mgr.GetToken("auth.json", 30*time.Second)
	if err == nil {
		t.Fatal("expected error after DeleteEntry")
	}
}

func TestGetToken_ZeroExpiresInSkipsCache(t *testing.T) {
	var callCount atomic.Int32
	var loadCount atomic.Int32
	var saveCount atomic.Int32

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		callCount.Add(1)
		s.AccessToken = "ephemeral-token"
		return 0, nil // expiresIn=0 means don't cache
	})
	// Override Load to return a valid storage and track calls
	cfg.Load = func(path string) (*testStorage, error) {
		loadCount.Add(1)
		return &testStorage{RefreshToken: "refresh-tok"}, nil
	}
	cfg.Save = func(path string, s *testStorage) error {
		saveCount.Add(1)
		return nil
	}
	mgr := NewManager(cfg)

	mgr.StoreEntry("auth.json", &testStorage{
		RefreshToken: "refresh-tok",
	}, time.Time{}, &http.Client{})

	// First call
	tok1, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("first GetToken error: %v", err)
	}
	if tok1 != "ephemeral-token" {
		t.Fatalf("token = %q, want %q", tok1, "ephemeral-token")
	}

	// Second call should re-enter refresh (not cached) and reload from disk
	_, _, err = mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("second GetToken error: %v", err)
	}
	if c := callCount.Load(); c != 2 {
		t.Fatalf("refresh calls = %d, want 2 (should not cache with expiresIn=0)", c)
	}
	// Second call should have reloaded from disk since cache was cleared
	if c := loadCount.Load(); c != 1 {
		t.Fatalf("load calls = %d, want 1 (second call should reload from disk)", c)
	}

	// Wait for any async goroutines
	time.Sleep(50 * time.Millisecond)

	// Save should never be called for expiresIn=0
	if c := saveCount.Load(); c != 0 {
		t.Fatalf("save calls = %d, want 0 (should skip save with expiresIn=0)", c)
	}
}

func TestGetToken_MultipleAuthFiles(t *testing.T) {
	var callCount atomic.Int32

	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		callCount.Add(1)
		s.AccessToken = "token-for-" + authFile
		return 1 * time.Hour, nil
	})
	mgr := NewManager(cfg)

	mgr.StoreEntry("auth1.json", &testStorage{RefreshToken: "ref1"}, time.Time{}, &http.Client{})
	mgr.StoreEntry("auth2.json", &testStorage{RefreshToken: "ref2"}, time.Time{}, &http.Client{})

	tok1, _, err := mgr.GetToken("auth1.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken auth1 error: %v", err)
	}
	tok2, _, err := mgr.GetToken("auth2.json", 30*time.Second)
	if err != nil {
		t.Fatalf("GetToken auth2 error: %v", err)
	}

	if tok1 != "token-for-auth1.json" {
		t.Fatalf("auth1 token = %q", tok1)
	}
	if tok2 != "token-for-auth2.json" {
		t.Fatalf("auth2 token = %q", tok2)
	}
	if c := callCount.Load(); c != 2 {
		t.Fatalf("refresh calls = %d, want 2 (one per auth file)", c)
	}
}

func TestGetToken_StaleWriteSkipped(t *testing.T) {
	var saveMu sync.Mutex
	var lastSavedToken string

	refreshCount := 0
	cfg := testConfig(func(client *http.Client, s *testStorage, authFile string) (time.Duration, error) {
		refreshCount++
		s.AccessToken = fmt.Sprintf("token-v%d", refreshCount)
		return 10 * time.Millisecond, nil
	})
	cfg.Save = func(path string, s *testStorage) error {
		// Simulate slow disk write for first save
		if s.AccessToken == "token-v1" {
			time.Sleep(100 * time.Millisecond)
		}
		saveMu.Lock()
		lastSavedToken = s.AccessToken
		saveMu.Unlock()
		return nil
	}
	mgr := NewManager(cfg)

	mgr.StoreEntry("auth.json", &testStorage{
		RefreshToken: "refresh-tok",
	}, time.Time{}, &http.Client{})

	// First refresh: dispatches async save with token-v1 (slow)
	_, _, err := mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("first GetToken error: %v", err)
	}

	// Wait for token to expire
	time.Sleep(20 * time.Millisecond)

	// Second refresh: dispatches async save with token-v2 (fast)
	_, _, err = mgr.GetToken("auth.json", 30*time.Second)
	if err != nil {
		t.Fatalf("second GetToken error: %v", err)
	}

	// Wait for all async saves to complete
	time.Sleep(200 * time.Millisecond)

	// The important invariant: the final state on disk must be the latest version.
	// Either v1 was skipped entirely, or v1 ran first and v2 overwrote it.
	saveMu.Lock()
	final := lastSavedToken
	saveMu.Unlock()
	if final != "token-v2" {
		t.Fatalf("final saved token = %q, want %q (stale write overwrote newer data)", final, "token-v2")
	}
}
