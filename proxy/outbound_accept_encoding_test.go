package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/missdeer/aiproxy/config"
)

func TestDoHTTPRequest_AcceptEncodingFromUpstreamConfig(t *testing.T) {
	tests := []struct {
		name        string
		compression string
		want        string
	}{
		{"zstd", "zstd", "zstd"},
		{"gzip", "gzip", "gzip"},
		{"none", "none", ""},
		{"empty defaults to zstd", "", "zstd"},
		{"unknown falls back to zstd", "snappy", "zstd"},
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

			upstream := config.Upstream{
				Name:        "test-upstream",
				BaseURL:     upstreamSrv.URL,
				Token:       "test-token",
				Compression: tt.compression,
			}

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

func TestDoHTTPRequestStream_AcceptEncodingFromUpstreamConfig(t *testing.T) {
	tests := []struct {
		name        string
		compression string
		want        string
	}{
		{"zstd", "zstd", "zstd"},
		{"none", "none", ""},
		{"unknown falls back to zstd", "snappy", "zstd"},
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

			upstream := config.Upstream{
				Name:        "test-upstream",
				BaseURL:     upstreamSrv.URL,
				Token:       "test-token",
				Compression: tt.compression,
			}

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
