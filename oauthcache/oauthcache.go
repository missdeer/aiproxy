// Package oauthcache provides a generic, high-performance OAuth token cache
// with singleflight deduplication, jitter-based expiry, async disk persistence,
// and in-memory refresh_token reuse.
//
// Usage:
//
//	mgr := oauthcache.NewManager(oauthcache.Config[MyStorage]{...})
//	token, storage, err := mgr.GetToken(authFile, timeout)
package oauthcache

import (
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// Config defines handler-specific callbacks for a particular storage type S.
type Config[S any] struct {
	// Label is the log prefix, e.g. "[CODEX]".
	Label string

	// Load reads a storage value from disk.
	Load func(path string) (*S, error)

	// Save writes a storage value to disk.
	Save func(path string, s *S) error

	// GetAccessToken extracts the access token string from the storage.
	GetAccessToken func(s *S) string

	// Copy returns a deep copy of the storage value.
	// For simple structs, a value copy suffices; for types with maps/slices,
	// this must perform a deep copy.
	Copy func(s *S) S

	// Refresh performs the HTTP token refresh, updating *s in-place.
	// It returns the token expiry duration and any error.
	// The caller already holds the write lock, so this runs exclusively.
	Refresh func(client *http.Client, s *S, authFile string) (expiresIn time.Duration, err error)
}

// entry manages the token lifecycle for a single auth file.
type entry[S any] struct {
	mu        sync.RWMutex
	sf        singleflight.Group
	authFile  string
	storage   *S
	expiresAt time.Time
	client    *http.Client
	saveMu    sync.Mutex    // serializes async disk writes
	saveVer   atomic.Uint64 // monotonic counter; stale writes are skipped
}

// Manager manages OAuth token caching for a specific handler type.
type Manager[S any] struct {
	cfg   Config[S]
	cache sync.Map // map[string]*entry[S]
}

// NewManager creates a new Manager with the given configuration.
func NewManager[S any](cfg Config[S]) *Manager[S] {
	return &Manager[S]{cfg: cfg}
}

// getEntry returns or creates an entry for the given auth file.
func (m *Manager[S]) getEntry(authFile string, timeout time.Duration) *entry[S] {
	if v, ok := m.cache.Load(authFile); ok {
		return v.(*entry[S])
	}
	e := &entry[S]{
		authFile: authFile,
		client:   &http.Client{Timeout: timeout},
	}
	actual, _ := m.cache.LoadOrStore(authFile, e)
	return actual.(*entry[S])
}

// result is the internal return type for singleflight.
type result[S any] struct {
	token   string
	storage *S
}

// GetToken returns a valid access token for the given auth file,
// refreshing if necessary. Safe for concurrent use.
func (m *Manager[S]) GetToken(authFile string, timeout time.Duration) (string, *S, error) {
	e := m.getEntry(authFile, timeout)

	// Fast path: read lock check
	e.mu.RLock()
	if e.storage != nil && time.Now().Before(e.expiresAt.Add(-60*time.Second)) {
		token := m.cfg.GetAccessToken(e.storage)
		s := m.cfg.Copy(e.storage)
		e.mu.RUnlock()
		return token, &s, nil
	}
	e.mu.RUnlock()

	// Slow path: use singleflight to merge concurrent refresh requests
	v, err, _ := e.sf.Do(authFile, func() (any, error) {
		e.mu.Lock()
		defer e.mu.Unlock()

		// Double check after acquiring write lock
		if e.storage != nil && time.Now().Before(e.expiresAt.Add(-60*time.Second)) {
			s := m.cfg.Copy(e.storage)
			return &result[S]{m.cfg.GetAccessToken(e.storage), &s}, nil
		}

		// Reuse in-memory storage if available, otherwise load from disk
		var storage *S
		if e.storage != nil {
			s := m.cfg.Copy(e.storage)
			storage = &s
		} else {
			var err error
			storage, err = m.cfg.Load(authFile)
			if err != nil {
				return nil, fmt.Errorf("load auth file %s: %w", authFile, err)
			}
		}

		// Perform the handler-specific refresh.
		// Each handler's Refresh function validates its own preconditions
		// (e.g., checking for refresh_token). This allows handlers like Gemini
		// to reuse a still-valid access_token without requiring a refresh_token.
		log.Printf("%s Refreshing token (file: %s)", m.cfg.Label, authFile)
		expiresIn, err := m.cfg.Refresh(e.client, storage, authFile)
		if err != nil {
			return nil, fmt.Errorf("token refresh for %s: %w", authFile, err)
		}

		// expiresIn <= 0 means "do not cache": clear in-memory state so the
		// next call reloads from disk, and skip the async save.
		if expiresIn <= 0 {
			e.storage = nil
			e.expiresAt = time.Time{}
			// Advance saveVer to suppress any pending stale writes from prior refreshes
			e.saveVer.Add(1)
			log.Printf("%s Token not cached (expiresIn=%v) for file: %s", m.cfg.Label, expiresIn, authFile)
		} else {
			// Async save to disk (serialized per entry, version-stamped to prevent stale writes)
			ver := e.saveVer.Add(1)
			go func(path string, s S, expectedVer uint64) {
				e.saveMu.Lock()
				defer e.saveMu.Unlock()
				if e.saveVer.Load() != expectedVer {
					return // a newer refresh has occurred, skip stale write
				}
				if err := m.cfg.Save(path, &s); err != nil {
					log.Printf("%s Warning: save %s: %v", m.cfg.Label, path, err)
				}
			}(authFile, m.cfg.Copy(storage), ver)

			// Cache with jitter to prevent synchronized expiration
			// Cap jitter to 10% of expiresIn (max 5min) to avoid immediate re-refresh for short TTLs
			maxJitter := expiresIn / 10
			if maxJitter > 5*time.Minute {
				maxJitter = 5 * time.Minute
			}
			jitter := time.Duration(0)
			if maxJitter > 0 {
				jitter = time.Duration(rand.Int63n(int64(maxJitter)))
			}
			e.storage = storage
			e.expiresAt = time.Now().Add(expiresIn - jitter)
			log.Printf("%s Token refreshed for file: %s", m.cfg.Label, authFile)
		}

		s := m.cfg.Copy(storage)
		return &result[S]{m.cfg.GetAccessToken(storage), &s}, nil
	})
	if err != nil {
		return "", nil, err
	}
	r := v.(*result[S])
	return r.token, r.storage, nil
}

// StoreEntry directly stores a cached entry (useful for testing).
func (m *Manager[S]) StoreEntry(authFile string, storage *S, expiresAt time.Time, client *http.Client) {
	e := &entry[S]{
		authFile:  authFile,
		storage:   storage,
		expiresAt: expiresAt,
		client:    client,
	}
	m.cache.Store(authFile, e)
}

// DeleteEntry removes a cached entry (useful for testing).
func (m *Manager[S]) DeleteEntry(authFile string) {
	m.cache.Delete(authFile)
}
