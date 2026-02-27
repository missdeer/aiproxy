package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCompressedTransport_DoesNotSetAcceptEncoding(t *testing.T) {
	var gotAcceptEncoding string
	tr := &CompressedTransport{
		Base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			gotAcceptEncoding = req.Header.Get("Accept-Encoding")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader([]byte("ok"))),
				Request:    req,
			}, nil
		}),
	}

	req, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if gotAcceptEncoding != "" {
		t.Fatalf("Accept-Encoding = %q, want empty", gotAcceptEncoding)
	}
}

func TestCompressedTransport_DecompressesSupportedEncodings(t *testing.T) {
	original := []byte("hello compressed transport")
	tests := []struct {
		name        string
		encoding    string
		compressFn  func([]byte) []byte
		contentType string
	}{
		{name: "gzip", encoding: "gzip", compressFn: mustGzip},
		{name: "zstd", encoding: "zstd", compressFn: mustZstd},
		{name: "brotli", encoding: "br", compressFn: mustBrotli},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compressed := tt.compressFn(original)
			tr := &CompressedTransport{
				Base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					h := make(http.Header)
					h.Set("Content-Encoding", tt.encoding)
					h.Set("Content-Length", "123")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     h,
						Body:       io.NopCloser(bytes.NewReader(compressed)),
						Request:    req,
					}, nil
				}),
			}

			req, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip() error = %v", err)
			}
			defer resp.Body.Close()

			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			if string(got) != string(original) {
				t.Fatalf("body = %q, want %q", got, original)
			}
			if resp.Header.Get("Content-Encoding") != "" {
				t.Fatalf("Content-Encoding should be removed after decompression")
			}
			if resp.Header.Get("Content-Length") != "" {
				t.Fatalf("Content-Length should be removed after decompression")
			}
		})
	}
}

func TestCompressedTransport_PassThroughScenarios(t *testing.T) {
	compressed := mustGzip([]byte("passthrough"))
	tests := []struct {
		name       string
		method     string
		statusCode int
		encoding   string
	}{
		{name: "HEAD request", method: http.MethodHead, statusCode: http.StatusOK, encoding: "gzip"},
		{name: "204 no content", method: http.MethodGet, statusCode: http.StatusNoContent, encoding: "gzip"},
		{name: "304 not modified", method: http.MethodGet, statusCode: http.StatusNotModified, encoding: "gzip"},
		{name: "multi encoding", method: http.MethodGet, statusCode: http.StatusOK, encoding: "gzip, br"},
		{name: "unknown encoding", method: http.MethodGet, statusCode: http.StatusOK, encoding: "snappy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &CompressedTransport{
				Base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					h := make(http.Header)
					h.Set("Content-Encoding", tt.encoding)
					h.Set("Content-Length", "123")
					return &http.Response{
						StatusCode: tt.statusCode,
						Header:     h,
						Body:       io.NopCloser(bytes.NewReader(compressed)),
						Request:    req,
					}, nil
				}),
			}

			req, _ := http.NewRequest(tt.method, "http://example.test", nil)
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip() error = %v", err)
			}
			defer resp.Body.Close()

			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			if !bytes.Equal(got, compressed) {
				t.Fatalf("body changed unexpectedly in passthrough path")
			}
			if resp.Header.Get("Content-Encoding") != tt.encoding {
				t.Fatalf("Content-Encoding = %q, want %q", resp.Header.Get("Content-Encoding"), tt.encoding)
			}
		})
	}
}

func TestCompressedTransport_StreamReadWithWrappedBody(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		zw := gzip.NewWriter(pw)
		zw.Write([]byte("chunk-1 "))
		zw.Flush()
		time.Sleep(5 * time.Millisecond)
		zw.Write([]byte("chunk-2"))
		zw.Close()
		pw.Close()
	}()

	tr := &CompressedTransport{
		Base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			h := make(http.Header)
			h.Set("Content-Encoding", "gzip")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     h,
				Body:       pr,
				Request:    req,
			}, nil
		}),
	}

	req, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != "chunk-1 chunk-2" {
		t.Fatalf("stream body = %q, want %q", got, "chunk-1 chunk-2")
	}
}

func TestWrapDecompressor_CloseClosesUnderlyingBody(t *testing.T) {
	body := &trackingReadCloser{Reader: bytes.NewReader(mustBrotli([]byte("x")))}
	rc, err := wrapDecompressor(body, "br")
	if err != nil {
		t.Fatalf("wrapDecompressor() error = %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !body.closed {
		t.Fatal("underlying body was not closed")
	}
}

func TestUnwrapTransport(t *testing.T) {
	base := &http.Transport{}
	nested := &CompressedTransport{Base: &CompressedTransport{Base: base}}

	if got := UnwrapTransport(nested); got != base {
		t.Fatalf("UnwrapTransport(nested) returned wrong transport")
	}
	if got := UnwrapTransport(base); got != base {
		t.Fatalf("UnwrapTransport(base) should return base transport")
	}
	if got := UnwrapTransport(&CompressedTransport{}); got != nil {
		t.Fatalf("UnwrapTransport(empty wrapped) = %v, want nil", got)
	}
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (t *trackingReadCloser) Close() error {
	t.closed = true
	return nil
}

func mustGzip(in []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(in); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func mustZstd(in []byte) []byte {
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		panic(err)
	}
	if _, err := zw.Write(in); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func mustBrotli(in []byte) []byte {
	var buf bytes.Buffer
	zw := brotli.NewWriter(&buf)
	if _, err := zw.Write(in); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
