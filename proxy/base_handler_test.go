package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/middleware"
)

func TestApplyAcceptEncoding(t *testing.T) {
	tests := []struct {
		name          string
		compression   string
		initialHeader string
		wantHeader    string
	}{
		{"zstd sets header", "zstd", "", "zstd"},
		{"gzip sets header", "gzip", "", "gzip"},
		{"br sets header", "br", "", "br"},
		{"none deletes header", "none", "gzip", ""},
		{"empty defaults to zstd", "", "", "zstd"},
		{"unknown falls back to zstd", "snappy", "", "zstd"},
		{"overrides existing client header", "gzip", "br", "gzip"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "http://example.test", nil)
			if tt.initialHeader != "" {
				req.Header.Set("Accept-Encoding", tt.initialHeader)
			}
			ApplyAcceptEncoding(req, config.Upstream{
				Name:        "test-upstream",
				Compression: tt.compression,
			})

			got := req.Header.Get("Accept-Encoding")
			if got != tt.wantHeader {
				t.Fatalf("Accept-Encoding = %q, want %q", got, tt.wantHeader)
			}
		})
	}
}

func TestClientResponseHeaderTimeout_WrappedTransport(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = 25 * time.Second

	client := &http.Client{
		Transport: &middleware.CompressedTransport{
			Base: &middleware.CompressedTransport{
				Base: base,
			},
		},
	}

	if got := ClientResponseHeaderTimeout(client); got != 25*time.Second {
		t.Fatalf("ClientResponseHeaderTimeout() = %v, want %v", got, 25*time.Second)
	}
}

func TestNewHTTPClient_UsesCompressedTransportAndDisableCompression(t *testing.T) {
	client := newHTTPClient(12 * time.Second)
	wrapped, ok := client.Transport.(*middleware.CompressedTransport)
	if !ok {
		t.Fatalf("client.Transport type = %T, want *middleware.CompressedTransport", client.Transport)
	}
	base := middleware.UnwrapTransport(wrapped)
	if base == nil {
		t.Fatal("expected underlying *http.Transport, got nil")
	}
	if !base.DisableCompression {
		t.Fatal("DisableCompression should be true")
	}
	if base.ResponseHeaderTimeout != 12*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", base.ResponseHeaderTimeout, 12*time.Second)
	}
}
