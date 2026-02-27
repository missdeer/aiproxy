package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDecompressionMiddleware_DecodesNormalizedMultiEncoding(t *testing.T) {
	original := []byte(`{"input":"hello"}`)
	encoded := mustBrotli(mustGzip(original)) // gzip then br => Content-Encoding: gzip, br

	var gotBody []byte
	var gotEncoding string
	var gotLength int64

	handler := DecompressionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		gotEncoding = r.Header.Get("Content-Encoding")
		gotLength = r.ContentLength
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(encoded))
	req.Header.Set("Content-Encoding", "  GZip ; level=9, Br ")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
	if !bytes.Equal(gotBody, original) {
		t.Fatalf("body = %q, want %q", gotBody, original)
	}
	if gotEncoding != "" {
		t.Fatalf("Content-Encoding = %q, want empty", gotEncoding)
	}
	if gotLength != int64(len(original)) {
		t.Fatalf("ContentLength = %d, want %d", gotLength, len(original))
	}
}

func TestDecompressionMiddleware_RejectsOversizedCompressedBody(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), int(maxCompressedRequestBodyBytes)+1)
	called := false

	handler := DecompressionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(oversized))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
	if called {
		t.Fatal("next handler should not be called for oversized body")
	}
}

func TestDecompressBody_RejectsOversizedDecompressedBody(t *testing.T) {
	original := bytes.Repeat([]byte("x"), 4096)
	compressed := mustGzip(original)

	_, err := decompressBody(compressed, "gzip", 1024)
	if !errors.Is(err, errDecompressedBodyTooLarge) {
		t.Fatalf("err = %v, want %v", err, errDecompressedBodyTooLarge)
	}
}

func TestDecompressBody_TrimsAndLowercasesSingleEncoding(t *testing.T) {
	original := []byte("payload")
	compressed := mustGzip(original)

	got, err := decompressBody(compressed, "  GZIP  ", maxDecompressedRequestBodyBytes)
	if err != nil {
		t.Fatalf("decompressBody() error = %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("body = %q, want %q", got, original)
	}
}
