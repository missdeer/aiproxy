package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func TestCompressBody_GzipRoundTrip(t *testing.T) {
	original := []byte("Hello, world! This is a test of gzip compression.")
	compressed, enc, err := CompressBody(original, "gzip")
	if err != nil {
		t.Fatalf("CompressBody gzip: %v", err)
	}
	if enc != "gzip" {
		t.Fatalf("expected encoding %q, got %q", "gzip", enc)
	}

	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer r.Close()
	decompressed, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(original, decompressed) {
		t.Fatalf("round-trip mismatch: got %q", decompressed)
	}
}

func TestCompressBody_ZstdRoundTrip(t *testing.T) {
	original := []byte("Hello, world! This is a test of zstd compression.")
	compressed, enc, err := CompressBody(original, "zstd")
	if err != nil {
		t.Fatalf("CompressBody zstd: %v", err)
	}
	if enc != "zstd" {
		t.Fatalf("expected encoding %q, got %q", "zstd", enc)
	}

	r, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer r.Close()
	decompressed, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(original, decompressed) {
		t.Fatalf("round-trip mismatch: got %q", decompressed)
	}
}

func TestCompressBody_BrotliRoundTrip(t *testing.T) {
	original := []byte("Hello, world! This is a test of brotli compression.")
	compressed, enc, err := CompressBody(original, "br")
	if err != nil {
		t.Fatalf("CompressBody br: %v", err)
	}
	if enc != "br" {
		t.Fatalf("expected encoding %q, got %q", "br", enc)
	}

	decompressed, err := io.ReadAll(brotli.NewReader(bytes.NewReader(compressed)))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(original, decompressed) {
		t.Fatalf("round-trip mismatch: got %q", decompressed)
	}
}

func TestCompressBody_NonePassthrough(t *testing.T) {
	for _, enc := range []string{"", "none", "identity"} {
		original := []byte("pass through data")
		result, retEnc, err := CompressBody(original, enc)
		if err != nil {
			t.Fatalf("CompressBody(%q): %v", enc, err)
		}
		if retEnc != "" {
			t.Fatalf("expected empty encoding for %q, got %q", enc, retEnc)
		}
		if !bytes.Equal(original, result) {
			t.Fatalf("passthrough mismatch for %q", enc)
		}
	}
}

func TestCompressBody_EmptyBody(t *testing.T) {
	for _, enc := range []string{"gzip", "zstd", "br"} {
		compressed, retEnc, err := CompressBody(nil, enc)
		if err != nil {
			t.Fatalf("CompressBody(%q, nil): %v", enc, err)
		}
		if retEnc == "" {
			t.Fatalf("expected non-empty encoding for %q with nil body", enc)
		}
		_ = compressed // compressed empty data is still valid
	}
}

func TestCompressBody_Unsupported(t *testing.T) {
	_, _, err := CompressBody([]byte("data"), "deflate")
	if err == nil {
		t.Fatal("expected error for unsupported encoding, got nil")
	}
}
