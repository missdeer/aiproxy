package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/oauthcache"
)

type authFileAttempt struct {
	AuthFile string
	Index    int
	IsLast   bool
}

func iterAuthFiles(upstream config.Upstream) ([]authFileAttempt, error) {
	files := append([]string(nil), upstream.AuthFiles...)
	n := len(files)
	if n == 0 {
		return nil, fmt.Errorf("no auth files configured for upstream %s", upstream.Name)
	}
	start := upstream.AuthFileStartIndex()
	attempts := make([]authFileAttempt, n)
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		attempts[i] = authFileAttempt{
			AuthFile: files[idx],
			Index:    idx,
			IsLast:   i == n-1,
		}
	}
	return attempts, nil
}

func clonePayload(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

type authRetryConfig[S any] struct {
	Label       string
	Manager     *oauthcache.Manager[S]
	Validate    func(storage *S, authFile string) error
	MarshalBody func(base map[string]any, storage *S) ([]byte, error)
	BuildReq    func(upstream config.Upstream, body []byte, token string, storage *S, ctx context.Context) (*http.Request, error)
	ConvertSSE  func(data []byte) ([]byte, error)
	NeedConvert func(headers http.Header, clientWantsStream bool) bool
}

func forwardWithAuthRetry[S any](
	cfg authRetryConfig[S],
	client *http.Client,
	upstream config.Upstream,
	basePayload map[string]any,
	clientWantsStream bool,
) (int, []byte, http.Header, error) {
	if cfg.Manager == nil {
		return 0, nil, nil, fmt.Errorf("%s invalid config: Manager is nil", cfg.Label)
	}
	if cfg.BuildReq == nil {
		return 0, nil, nil, fmt.Errorf("%s invalid config: BuildReq is nil", cfg.Label)
	}

	attempts, err := iterAuthFiles(upstream)
	if err != nil {
		return 0, nil, nil, err
	}

	timeout := ClientResponseHeaderTimeout(client)
	var lastErr error
	var lastStatus int
	var lastBody []byte
	var lastHeaders http.Header

	for i, attempt := range attempts {
		token, storage, err := cfg.Manager.GetToken(attempt.AuthFile, timeout)
		if err != nil {
			log.Printf("%s Auth file %s (%d/%d): %v", cfg.Label, attempt.AuthFile, i+1, len(attempts), err)
			lastErr = err
			continue
		}

		if cfg.Validate != nil {
			if err := cfg.Validate(storage, attempt.AuthFile); err != nil {
				log.Printf("%s Auth file %s (%d/%d): %v", cfg.Label, attempt.AuthFile, i+1, len(attempts), err)
				lastErr = err
				continue
			}
		}

		var body []byte
		if cfg.MarshalBody != nil {
			body, err = cfg.MarshalBody(basePayload, storage)
		} else {
			body, err = json.Marshal(basePayload)
		}
		if err != nil {
			lastErr = err
			continue
		}

		req, err := cfg.BuildReq(upstream, body, token, storage, context.Background())
		if err != nil {
			lastErr = err
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("%s Auth file %s (%d/%d): request failed: %v", cfg.Label, attempt.AuthFile, i+1, len(attempts), err)
			lastErr = err
			lastStatus = http.StatusBadGateway
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			lastStatus = http.StatusBadGateway
			continue
		}

		headers := resp.Header.Clone()
		StripHopByHopHeaders(headers)
		headers.Del("Content-Length")
		headers.Del("Content-Encoding")

		if resp.StatusCode >= 400 {
			log.Printf("%s Auth file %s (%d/%d): HTTP %d", cfg.Label, attempt.AuthFile, i+1, len(attempts), resp.StatusCode)
			lastErr = fmt.Errorf("upstream returned status %d", resp.StatusCode)
			lastStatus = resp.StatusCode
			lastBody = respBody
			lastHeaders = headers
			continue
		}

		needConvert := !clientWantsStream
		if cfg.NeedConvert != nil {
			needConvert = cfg.NeedConvert(headers, clientWantsStream)
		}
		if needConvert {
			if cfg.ConvertSSE == nil {
				return 0, nil, nil, fmt.Errorf("%s NeedConvert is true but ConvertSSE is nil", cfg.Label)
			}
			jsonResp, err := cfg.ConvertSSE(respBody)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("%s SSE conversion: %w", cfg.Label, err)
			}
			headers.Set("Content-Type", "application/json")
			return resp.StatusCode, jsonResp, headers, nil
		}
		return resp.StatusCode, respBody, headers, nil
	}

	if lastBody != nil {
		return lastStatus, lastBody, lastHeaders, nil
	}
	return lastStatus, nil, nil, lastErr
}

func forwardStreamWithAuthRetry[S any](
	cfg authRetryConfig[S],
	client *http.Client,
	upstream config.Upstream,
	basePayload map[string]any,
	ctx context.Context,
) (*http.Response, error) {
	if cfg.Manager == nil {
		return nil, fmt.Errorf("%s invalid config: Manager is nil", cfg.Label)
	}
	if cfg.BuildReq == nil {
		return nil, fmt.Errorf("%s invalid config: BuildReq is nil", cfg.Label)
	}

	attempts, err := iterAuthFiles(upstream)
	if err != nil {
		return nil, err
	}

	timeout := ClientResponseHeaderTimeout(client)
	var lastErr error
	var lastResp *http.Response

	for i, attempt := range attempts {
		token, storage, err := cfg.Manager.GetToken(attempt.AuthFile, timeout)
		if err != nil {
			log.Printf("%s Auth file %s (%d/%d): %v", cfg.Label, attempt.AuthFile, i+1, len(attempts), err)
			lastErr = err
			continue
		}

		if cfg.Validate != nil {
			if err := cfg.Validate(storage, attempt.AuthFile); err != nil {
				log.Printf("%s Auth file %s (%d/%d): %v", cfg.Label, attempt.AuthFile, i+1, len(attempts), err)
				lastErr = err
				continue
			}
		}

		var body []byte
		if cfg.MarshalBody != nil {
			body, err = cfg.MarshalBody(basePayload, storage)
		} else {
			body, err = json.Marshal(basePayload)
		}
		if err != nil {
			lastErr = err
			continue
		}

		req, err := cfg.BuildReq(upstream, body, token, storage, ctx)
		if err != nil {
			lastErr = err
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("%s Auth file %s (%d/%d): request failed: %v", cfg.Label, attempt.AuthFile, i+1, len(attempts), err)
			lastErr = err
			continue
		}

		if resp.StatusCode >= 400 {
			log.Printf("%s Auth file %s (%d/%d): HTTP %d", cfg.Label, attempt.AuthFile, i+1, len(attempts), resp.StatusCode)
			lastErr = fmt.Errorf("upstream returned status %d", resp.StatusCode)
			if lastResp != nil {
				lastResp.Body.Close()
			}
			lastResp = resp
			continue
		}

		if lastResp != nil {
			lastResp.Body.Close()
			lastResp = nil
		}
		return resp, nil
	}

	if lastResp != nil {
		return lastResp, nil
	}
	return nil, lastErr
}
