package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/missdeer/aiproxy/config"
)

func TestApplyCustomHeaders(t *testing.T) {
	t.Run("nil map is no-op", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://example.test", nil)
		req.Header.Set("Content-Type", "application/json")
		upstream := config.Upstream{Name: "test"}
		ApplyCustomHeaders(req, upstream)
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want %q", got, "application/json")
		}
	})

	t.Run("adds new headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://example.test", nil)
		upstream := config.Upstream{
			Name:        "test",
			HTTPHeaders: map[string]string{"X-Custom": "value"},
		}
		ApplyCustomHeaders(req, upstream)
		if got := req.Header.Get("X-Custom"); got != "value" {
			t.Fatalf("X-Custom = %q, want %q", got, "value")
		}
	})

	t.Run("overrides existing headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://example.test", nil)
		req.Header.Set("Authorization", "Bearer original")
		upstream := config.Upstream{
			Name:        "test",
			HTTPHeaders: map[string]string{"Authorization": "Bearer override"},
		}
		ApplyCustomHeaders(req, upstream)
		if got := req.Header.Get("Authorization"); got != "Bearer override" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer override")
		}
	})

	t.Run("multiple headers applied", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://example.test", nil)
		upstream := config.Upstream{
			Name: "test",
			HTTPHeaders: map[string]string{
				"X-First":  "1",
				"X-Second": "2",
			},
		}
		ApplyCustomHeaders(req, upstream)
		if got := req.Header.Get("X-First"); got != "1" {
			t.Fatalf("X-First = %q, want %q", got, "1")
		}
		if got := req.Header.Get("X-Second"); got != "2" {
			t.Fatalf("X-Second = %q, want %q", got, "2")
		}
	})

	t.Run("host sets req.Host", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://example.test", nil)
		upstream := config.Upstream{
			Name:        "test",
			HTTPHeaders: map[string]string{"Host": "upstream.internal"},
		}
		ApplyCustomHeaders(req, upstream)
		if req.Host != "upstream.internal" {
			t.Fatalf("req.Host = %q, want %q", req.Host, "upstream.internal")
		}
	})
}
