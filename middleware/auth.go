package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/missdeer/aiproxy/config"
)

type AuthMiddleware struct {
	keyDigests atomic.Pointer[[][32]byte]
}

func NewAuthMiddleware(cfg *config.Config) *AuthMiddleware {
	m := &AuthMiddleware{}
	m.UpdateConfig(cfg)
	return m
}

func (m *AuthMiddleware) UpdateConfig(cfg *config.Config) {
	// Precompute SHA-256 digests for constant-time comparison
	if len(cfg.APIKeys) > 0 {
		digests := make([][32]byte, len(cfg.APIKeys))
		for i, key := range cfg.APIKeys {
			digests[i] = sha256.Sum256([]byte(key))
		}
		m.keyDigests.Store(&digests)
	} else {
		m.keyDigests.Store(nil)
	}
}

func (m *AuthMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		digests := m.keyDigests.Load()

		// If no API keys configured, allow all requests
		if digests == nil {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		if authHeader == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Parse Authorization header (case-insensitive scheme, handle whitespace)
		scheme, token, ok := strings.Cut(authHeader, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		token = strings.TrimSpace(token)
		if token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Constant-time comparison using SHA-256 digests
		tokenDigest := sha256.Sum256([]byte(token))
		matched := 0
		for _, keyDigest := range *digests {
			matched |= subtle.ConstantTimeCompare(tokenDigest[:], keyDigest[:])
		}

		if matched == 1 {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}
