package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/missdeer/aiproxy/config"
)

func TestApplyBodyCompression_NoneNormalizesRequestState(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.test", bytes.NewReader([]byte("old")))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Length", "3")

	body := []byte(`{"k":"v"}`)
	upstream := config.Upstream{Name: "test", RequestCompression: "none"}

	finalBody, err := ApplyBodyCompression(req, body, upstream)
	if err != nil {
		t.Fatalf("ApplyBodyCompression() error = %v", err)
	}
	if !bytes.Equal(finalBody, body) {
		t.Fatalf("final body = %q, want %q", finalBody, body)
	}
	if req.Header.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding = %q, want empty", req.Header.Get("Content-Encoding"))
	}
	if req.ContentLength != int64(len(body)) {
		t.Fatalf("ContentLength = %d, want %d", req.ContentLength, len(body))
	}
	if req.Header.Get("Content-Length") != "9" {
		t.Fatalf("Content-Length = %q, want %q", req.Header.Get("Content-Length"), "9")
	}

	gotBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll(req.Body) error = %v", err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("req body = %q, want %q", gotBody, body)
	}

	replay, err := req.GetBody()
	if err != nil {
		t.Fatalf("req.GetBody() error = %v", err)
	}
	defer replay.Close()
	replayBody, err := io.ReadAll(replay)
	if err != nil {
		t.Fatalf("ReadAll(replay) error = %v", err)
	}
	if !bytes.Equal(replayBody, body) {
		t.Fatalf("replay body = %q, want %q", replayBody, body)
	}
}

func TestDoHTTPRequest_RequestBodyCompressionFromUpstreamConfig(t *testing.T) {
	tests := []struct {
		name               string
		requestCompression string
		wantEncoding       string
	}{
		{name: "none", requestCompression: "none", wantEncoding: ""},
		{name: "gzip", requestCompression: "gzip", wantEncoding: "gzip"},
		{name: "zstd", requestCompression: "zstd", wantEncoding: "zstd"},
		{name: "brotli", requestCompression: "br", wantEncoding: "br"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				gotEncoding      string
				gotRawBody       []byte
				gotContentLength int64
			)

			upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotEncoding = r.Header.Get("Content-Encoding")
				gotContentLength = r.ContentLength
				var err error
				gotRawBody, err = io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("ReadAll(r.Body) error = %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer upstreamSrv.Close()

			original := []byte(`{"input":"` + strings.Repeat("hello-", 300) + `"}`)
			originalReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			upstream := config.Upstream{
				Name:               "test-upstream",
				BaseURL:            upstreamSrv.URL,
				Token:              "test-token",
				RequestCompression: tt.requestCompression,
			}

			client := newHTTPClient(10 * time.Second)
			status, _, _, err := doHTTPRequest(client, upstreamSrv.URL, original, upstream, config.APITypeOpenAI, originalReq, "")
			if err != nil {
				t.Fatalf("doHTTPRequest() error = %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			if gotEncoding != tt.wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", gotEncoding, tt.wantEncoding)
			}
			if gotContentLength != int64(len(gotRawBody)) {
				t.Fatalf("ContentLength = %d, want %d", gotContentLength, len(gotRawBody))
			}

			gotDecoded := decodeRequestBodyForTest(t, gotRawBody, gotEncoding)
			if !bytes.Equal(gotDecoded, original) {
				t.Fatalf("decoded body = %q, want %q", gotDecoded, original)
			}
		})
	}
}

func TestDoHTTPRequestStream_RequestBodyCompressionFromUpstreamConfig(t *testing.T) {
	tests := []struct {
		name               string
		requestCompression string
		wantEncoding       string
	}{
		{name: "none", requestCompression: "none", wantEncoding: ""},
		{name: "gzip", requestCompression: "gzip", wantEncoding: "gzip"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				gotEncoding      string
				gotRawBody       []byte
				gotContentLength int64
			)

			upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotEncoding = r.Header.Get("Content-Encoding")
				gotContentLength = r.ContentLength
				var err error
				gotRawBody, err = io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("ReadAll(r.Body) error = %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("data: ok\n\n"))
			}))
			defer upstreamSrv.Close()

			original := []byte(`{"input":"` + strings.Repeat("stream-", 300) + `"}`)
			originalReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			upstream := config.Upstream{
				Name:               "test-upstream",
				BaseURL:            upstreamSrv.URL,
				Token:              "test-token",
				RequestCompression: tt.requestCompression,
			}

			client := newHTTPClient(10 * time.Second)
			resp, err := doHTTPRequestStream(client, upstreamSrv.URL, original, upstream, config.APITypeOpenAI, originalReq, "")
			if err != nil {
				t.Fatalf("doHTTPRequestStream() error = %v", err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if gotEncoding != tt.wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", gotEncoding, tt.wantEncoding)
			}
			if gotContentLength != int64(len(gotRawBody)) {
				t.Fatalf("ContentLength = %d, want %d", gotContentLength, len(gotRawBody))
			}
			gotDecoded := decodeRequestBodyForTest(t, gotRawBody, gotEncoding)
			if !bytes.Equal(gotDecoded, original) {
				t.Fatalf("decoded body = %q, want %q", gotDecoded, original)
			}
		})
	}
}

func decodeRequestBodyForTest(t *testing.T, body []byte, encoding string) []byte {
	t.Helper()

	switch encoding {
	case "":
		return body
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("gzip.NewReader() error = %v", err)
		}
		defer r.Close()
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("gzip ReadAll() error = %v", err)
		}
		return out
	case "zstd":
		r, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("zstd.NewReader() error = %v", err)
		}
		defer r.Close()
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("zstd ReadAll() error = %v", err)
		}
		return out
	case "br":
		out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(body)))
		if err != nil {
			t.Fatalf("brotli ReadAll() error = %v", err)
		}
		return out
	default:
		t.Fatalf("unsupported test encoding: %s", encoding)
		return nil
	}
}
