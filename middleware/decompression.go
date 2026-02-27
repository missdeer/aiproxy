package middleware

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const (
	maxCompressedRequestBodyBytes   int64 = 16 << 20 // 16 MiB
	maxDecompressedRequestBodyBytes int64 = 64 << 20 // 64 MiB
)

var (
	errRequestBodyTooLarge      = errors.New("request body too large")
	errDecompressedBodyTooLarge = errors.New("decompressed body too large")
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

		// Read the entire body with an upper bound to avoid memory exhaustion.
		body, err := readAllWithLimit(r.Body, maxCompressedRequestBodyBytes, errRequestBodyTooLarge)
		r.Body.Close()
		if err != nil {
			if errors.Is(err, errRequestBodyTooLarge) {
				http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			log.Printf("[ERROR] Failed to read request body: %v", err)
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			return
		}

		// Decompress if needed
		decompressed, err := decompressBody(body, r.Header.Get("Content-Encoding"), maxDecompressedRequestBodyBytes)
		if err != nil {
			if errors.Is(err, errDecompressedBodyTooLarge) {
				http.Error(w, "Decompressed request body too large", http.StatusRequestEntityTooLarge)
				return
			}
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

func decompressBody(body []byte, contentEncoding string, maxDecompressedSize int64) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	encodings := parseContentEncodings(contentEncoding)
	if len(encodings) == 0 {
		// Auto-detect compression format by magic bytes when Content-Encoding is missing.
		if detected := detectCompressionFormat(body); detected != "" {
			encodings = []string{detected}
		}
	}

	if len(encodings) == 0 {
		return body, nil
	}

	decompressed := body
	decompressedCodings := make([]string, 0, len(encodings))
	for i := len(encodings) - 1; i >= 0; i-- {
		coding := encodings[i]
		switch coding {
		case "", "identity", "none":
			continue
		default:
			var err error
			decompressed, err = decompressByEncoding(decompressed, coding, maxDecompressedSize)
			if err != nil {
				return nil, err
			}
			decompressedCodings = append(decompressedCodings, coding)
		}
	}

	if len(decompressedCodings) > 0 {
		log.Printf("[DECOMPRESS] %s: %d bytes -> %d bytes",
			strings.Join(decompressedCodings, ","), len(body), len(decompressed))
	}

	return decompressed, nil
}

func parseContentEncodings(contentEncoding string) []string {
	if contentEncoding == "" {
		return nil
	}

	parts := strings.Split(contentEncoding, ",")
	encodings := make([]string, 0, len(parts))
	for _, part := range parts {
		encoding := strings.ToLower(strings.TrimSpace(part))
		if encoding == "" {
			continue
		}
		// Tolerate accidental parameters and strip them.
		if sep := strings.IndexByte(encoding, ';'); sep >= 0 {
			encoding = strings.TrimSpace(encoding[:sep])
		}
		if encoding == "" {
			continue
		}
		encodings = append(encodings, encoding)
	}

	return encodings
}

func decompressByEncoding(body []byte, encoding string, maxDecompressedSize int64) ([]byte, error) {
	switch encoding {
	case "gzip", "x-gzip":
		gzReader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create gzip reader: %w", err)
		}
		defer gzReader.Close()
		decompressed, err := readAllWithLimit(gzReader, maxDecompressedSize, errDecompressedBodyTooLarge)
		if err != nil {
			return nil, fmt.Errorf("decompress gzip: %w", err)
		}
		return decompressed, nil

	case "zstd", "x-zstd":
		zstdReader, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create zstd reader: %w", err)
		}
		defer zstdReader.Close()
		decompressed, err := readAllWithLimit(zstdReader, maxDecompressedSize, errDecompressedBodyTooLarge)
		if err != nil {
			return nil, fmt.Errorf("decompress zstd: %w", err)
		}
		return decompressed, nil

	case "br":
		brReader := brotli.NewReader(bytes.NewReader(body))
		decompressed, err := readAllWithLimit(brReader, maxDecompressedSize, errDecompressedBodyTooLarge)
		if err != nil {
			return nil, fmt.Errorf("decompress brotli: %w", err)
		}
		return decompressed, nil

	default:
		return nil, fmt.Errorf("unsupported Content-Encoding: %s", encoding)
	}
}

func readAllWithLimit(r io.Reader, limit int64, limitErr error) ([]byte, error) {
	limited := io.LimitReader(r, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, limitErr
	}
	return body, nil
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
