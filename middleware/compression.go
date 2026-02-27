package middleware

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// CompressBody compresses data using encoding ("gzip", "zstd", "br").
// Returns compressed bytes and normalized content-encoding value.
// For ""/  "none"/"identity", returns original bytes and empty encoding.
func CompressBody(data []byte, encoding string) ([]byte, string, error) {
	encoding = strings.TrimSpace(strings.ToLower(encoding))

	switch encoding {
	case "", "none", "identity":
		return data, "", nil
	case "gzip", "x-gzip":
		compressed, ce, err := compressGzip(data)
		if err != nil {
			return nil, "", err
		}
		return compressed, ce, nil
	case "zstd", "x-zstd":
		compressed, ce, err := compressZstd(data)
		if err != nil {
			return nil, "", err
		}
		return compressed, ce, nil
	case "br":
		compressed, ce, err := compressBrotli(data)
		if err != nil {
			return nil, "", err
		}
		return compressed, ce, nil
	default:
		return nil, "", fmt.Errorf("unsupported request compression: %s", encoding)
	}
}

func compressGzip(data []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, "", fmt.Errorf("gzip write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), "gzip", nil
}

func compressZstd(data []byte) ([]byte, string, error) {
	w, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, "", fmt.Errorf("create zstd writer: %w", err)
	}
	defer w.Close()
	compressed := w.EncodeAll(data, nil)
	return compressed, "zstd", nil
}

func compressBrotli(data []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, "", fmt.Errorf("brotli write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("brotli close: %w", err)
	}
	return buf.Bytes(), "br", nil
}
