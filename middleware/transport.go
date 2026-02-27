package middleware

import (
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

// CompressedTransport wraps a base transport and transparently decompresses
// compressed upstream responses. It does not set Accept-Encoding on requests.
type CompressedTransport struct {
	Base http.RoundTripper
}

func (t *CompressedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	if req.Method == http.MethodHead ||
		resp.StatusCode == http.StatusNoContent ||
		resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}

	raw := resp.Header.Get("Content-Encoding")
	if strings.Contains(raw, ",") {
		return resp, nil
	}

	encoding := strings.ToLower(strings.TrimSpace(raw))
	if encoding == "" || encoding == "identity" {
		return resp, nil
	}

	wrapped, err := wrapDecompressor(resp.Body, encoding)
	if err != nil {
		log.Printf("[TRANSPORT] decompressor init failed for %q: %v; passing through unchanged", encoding, err)
		return resp, nil
	}

	resp.Body = wrapped
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return resp, nil
}

func wrapDecompressor(rc io.ReadCloser, encoding string) (io.ReadCloser, error) {
	switch encoding {
	case "gzip", "x-gzip":
		reader, err := gzip.NewReader(rc)
		if err != nil {
			return nil, fmt.Errorf("create gzip reader: %w", err)
		}
		return &combinedReadCloser{
			reader: reader,
			body:   rc,
			closef: reader.Close,
		}, nil
	case "zstd":
		reader, err := zstd.NewReader(rc)
		if err != nil {
			return nil, fmt.Errorf("create zstd reader: %w", err)
		}
		return &combinedReadCloser{
			reader: reader,
			body:   rc,
			closef: func() error {
				reader.Close()
				return nil
			},
		}, nil
	case "br":
		return &combinedReadCloser{
			reader: brotli.NewReader(rc),
			body:   rc,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported content-encoding: %s", encoding)
	}
}

type combinedReadCloser struct {
	reader io.Reader
	body   io.Closer
	closef func() error
}

func (c *combinedReadCloser) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *combinedReadCloser) Close() error {
	var errReader error
	var errBody error

	if c.closef != nil {
		errReader = c.closef()
	}
	if c.body != nil {
		errBody = c.body.Close()
	}

	return errors.Join(errReader, errBody)
}

// UnwrapTransport recursively unwraps known transport wrappers and returns
// the underlying *http.Transport when available.
func UnwrapTransport(rt http.RoundTripper) *http.Transport {
	switch t := rt.(type) {
	case nil:
		return nil
	case *http.Transport:
		return t
	case *CompressedTransport:
		return UnwrapTransport(t.Base)
	default:
		return nil
	}
}
