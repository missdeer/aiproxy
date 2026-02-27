package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/missdeer/aiproxy/config"
)

func TestDoHTTPRequest_AcceptEncodingFixedCapabilities(t *testing.T) {
	tests := []struct {
		name     string
		upstream config.Upstream
		want     string
	}{
		{"default", config.Upstream{Name: "test-upstream"}, "gzip, zstd, br, identity"},
		{"same value when upstream set", config.Upstream{Name: "test-upstream"}, "gzip, zstd, br, identity"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAcceptEncoding string
			upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAcceptEncoding = r.Header.Get("Accept-Encoding")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"ok":true}`))
			}))
			defer upstreamSrv.Close()

			originalReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			originalReq.Header.Set("Accept-Encoding", "br")

			upstream := tt.upstream
			upstream.BaseURL = upstreamSrv.URL
			upstream.Token = "test-token"

			client := newHTTPClient(10 * time.Second)
			status, _, _, err := doHTTPRequest(client, upstreamSrv.URL, []byte(`{}`), upstream, config.APITypeOpenAI, originalReq, "")
			if err != nil {
				t.Fatalf("doHTTPRequest() error = %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			if gotAcceptEncoding != tt.want {
				t.Fatalf("Accept-Encoding = %q, want %q", gotAcceptEncoding, tt.want)
			}
		})
	}
}

func TestDoHTTPRequestStream_AcceptEncodingFixedCapabilities(t *testing.T) {
	tests := []struct {
		name     string
		upstream config.Upstream
		want     string
	}{
		{"default", config.Upstream{Name: "test-upstream"}, "gzip, zstd, br, identity"},
		{"same value when upstream set", config.Upstream{Name: "test-upstream"}, "gzip, zstd, br, identity"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAcceptEncoding string
			upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAcceptEncoding = r.Header.Get("Accept-Encoding")
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("data: ok\n\n"))
			}))
			defer upstreamSrv.Close()

			originalReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			originalReq.Header.Set("Accept-Encoding", "br")

			upstream := tt.upstream
			upstream.BaseURL = upstreamSrv.URL
			upstream.Token = "test-token"

			client := newHTTPClient(10 * time.Second)
			resp, err := doHTTPRequestStream(client, upstreamSrv.URL, []byte(`{}`), upstream, config.APITypeOpenAI, originalReq, "")
			if err != nil {
				t.Fatalf("doHTTPRequestStream() error = %v", err)
			}
			io.ReadAll(resp.Body)
			resp.Body.Close()

			if gotAcceptEncoding != tt.want {
				t.Fatalf("Accept-Encoding = %q, want %q", gotAcceptEncoding, tt.want)
			}
		})
	}
}
