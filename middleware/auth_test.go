package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/missdeer/aiproxy/config"
)

func TestAuthMiddleware(t *testing.T) {
	tests := []struct {
		name           string
		apiKeys        []string
		authHeader     string
		expectedStatus int
	}{
		{
			name:           "No API keys configured - allow all",
			apiKeys:        []string{},
			authHeader:     "",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "Valid API key",
			apiKeys:        []string{"test-key-1", "test-key-2"},
			authHeader:     "Bearer test-key-1",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "Valid API key (second key)",
			apiKeys:        []string{"test-key-1", "test-key-2"},
			authHeader:     "Bearer test-key-2",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "Invalid API key",
			apiKeys:        []string{"test-key-1", "test-key-2"},
			authHeader:     "Bearer invalid-key",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "Missing Authorization header",
			apiKeys:        []string{"test-key-1"},
			authHeader:     "",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "Invalid Authorization format (no Bearer prefix)",
			apiKeys:        []string{"test-key-1"},
			authHeader:     "test-key-1",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "Case-insensitive Bearer prefix (lowercase)",
			apiKeys:        []string{"test-key-1"},
			authHeader:     "bearer test-key-1",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "Case-insensitive Bearer prefix (mixed case)",
			apiKeys:        []string{"test-key-1"},
			authHeader:     "BeArEr test-key-1",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "Extra whitespace in header",
			apiKeys:        []string{"test-key-1"},
			authHeader:     "  Bearer   test-key-1  ",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "Empty token after Bearer",
			apiKeys:        []string{"test-key-1"},
			authHeader:     "Bearer   ",
			expectedStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				APIKeys: tt.apiKeys,
			}

			authMiddleware := NewAuthMiddleware(cfg)

			handler := authMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest("POST", "/v1/messages", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, rr.Code)
			}

			// Check WWW-Authenticate header for 401 responses
			if rr.Code == http.StatusUnauthorized {
				wwwAuth := rr.Header().Get("WWW-Authenticate")
				if wwwAuth != "Bearer" {
					t.Errorf("Expected WWW-Authenticate: Bearer, got %q", wwwAuth)
				}
			}
		})
	}
}

func TestAuthMiddleware_ConfigUpdate(t *testing.T) {
	cfg := &config.Config{
		APIKeys: []string{"old-key"},
	}

	authMiddleware := NewAuthMiddleware(cfg)

	handler := authMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Test with old key
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer old-key")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200 with old key, got %d", rr.Code)
	}

	// Update config with new key
	newCfg := &config.Config{
		APIKeys: []string{"new-key"},
	}
	authMiddleware.UpdateConfig(newCfg)

	// Test with old key (should fail)
	req = httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer old-key")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 with old key after update, got %d", rr.Code)
	}

	// Test with new key (should succeed)
	req = httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer new-key")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200 with new key, got %d", rr.Code)
	}
}
