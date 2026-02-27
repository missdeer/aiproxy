package middleware

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// DecompressionMiddleware is an HTTP middleware that automatically decompresses
// request bodies based on Content-Encoding header or magic bytes detection.
// Supports: gzip, zstd, br (brotli), and uncompressed (none).
func DecompressionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.ContentLength == 0 {
			next.ServeHTTP(w, r)
			return
		}

		// Read the entire body
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			log.Printf("[ERROR] Failed to read request body: %v", err)
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			return
		}

		// Decompress if needed
		decompressed, err := decompressBody(body, r.Header.Get("Content-Encoding"))
		if err != nil {
			log.Printf("[ERROR] Failed to decompress request body: %v", err)
			http.Error(w, "Failed to decompress request body", http.StatusBadRequest)
			return
		}

		// Replace body with decompressed content
		r.Body = io.NopCloser(bytes.NewReader(decompressed))
		r.ContentLength = int64(len(decompressed))
		// Remove Content-Encoding header since body is now decompressed
		r.Header.Del("Content-Encoding")

		next.ServeHTTP(w, r)
	})
}

// decompressBody decompresses the request body based on Content-Encoding header or magic bytes.
// Supports: gzip, zstd, br (brotli), and none (uncompressed).
func decompressBody(body []byte, contentEncoding string) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	// Auto-detect compression format by magic bytes if Content-Encoding not set
	if contentEncoding == "" {
		contentEncoding = detectCompressionFormat(body)
	}

	originalSize := len(body)

	switch contentEncoding {
	case "gzip":
		gzReader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create gzip reader: %w", err)
		}
		defer gzReader.Close()
		decompressed, err := io.ReadAll(gzReader)
		if err != nil {
			return nil, fmt.Errorf("decompress gzip: %w", err)
		}
		log.Printf("[DECOMPRESS] gzip: %d bytes -> %d bytes", originalSize, len(decompressed))
		return decompressed, nil

	case "zstd":
		zstdReader, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create zstd reader: %w", err)
		}
		defer zstdReader.Close()
		decompressed, err := io.ReadAll(zstdReader)
		if err != nil {
			return nil, fmt.Errorf("decompress zstd: %w", err)
		}
		log.Printf("[DECOMPRESS] zstd: %d bytes -> %d bytes", originalSize, len(decompressed))
		return decompressed, nil

	case "br":
		brReader := brotli.NewReader(bytes.NewReader(body))
		decompressed, err := io.ReadAll(brReader)
		if err != nil {
			return nil, fmt.Errorf("decompress brotli: %w", err)
		}
		log.Printf("[DECOMPRESS] brotli: %d bytes -> %d bytes", originalSize, len(decompressed))
		return decompressed, nil

	case "", "identity", "none":
		// No compression or explicitly uncompressed
		return body, nil

	default:
		return nil, fmt.Errorf("unsupported Content-Encoding: %s", contentEncoding)
	}
}

// detectCompressionFormat detects compression format by magic bytes
func detectCompressionFormat(body []byte) string {
	if len(body) < 4 {
		return ""
	}

	// gzip: 0x1f 0x8b
	if body[0] == 0x1f && body[1] == 0x8b {
		return "gzip"
	}

	// zstd: 0x28 0xb5 0x2f 0xfd
	if body[0] == 0x28 && body[1] == 0xb5 && body[2] == 0x2f && body[3] == 0xfd {
		return "zstd"
	}

	// brotli doesn't have a reliable magic number, rely on Content-Encoding header

	return ""
}
