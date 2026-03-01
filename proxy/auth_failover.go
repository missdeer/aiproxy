package proxy

import (
	"fmt"

	"github.com/missdeer/aiproxy/config"
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
