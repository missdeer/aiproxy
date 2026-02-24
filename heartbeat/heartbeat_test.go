package heartbeat

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/upstreammeta"
)

func TestHeartbeatUserAgentForAPIType(t *testing.T) {
	tests := []struct {
		name    string
		apiType config.APIType
		want    string
	}{
		{
			name:    "responses uses codex user agent",
			apiType: config.APITypeResponses,
			want:    upstreammeta.UserAgentCodexCLI,
		},
		{
			name:    "openai uses codex user agent",
			apiType: config.APITypeOpenAI,
			want:    upstreammeta.UserAgentCodexCLI,
		},
		{
			name:    "gemini uses gemini cli user agent",
			apiType: config.APITypeGemini,
			want:    upstreammeta.UserAgentGeminiCLI,
		},
		{
			name:    "anthropic uses claude code user agent",
			apiType: config.APITypeAnthropic,
			want:    upstreammeta.UserAgentClaudeCodeCLI,
		},
		{
			name:    "unknown api type has no user agent override",
			apiType: "unknown",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upstreammeta.HeartbeatUserAgentForAPIType(tt.apiType)
			if got != tt.want {
				t.Fatalf("HeartbeatUserAgentForAPIType(%q) = %q, want %q", tt.apiType, got, tt.want)
			}
		})
	}
}

func TestSendHeartbeatResponsesSetsCodexUserAgent(t *testing.T) {
	var gotUserAgent string
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	mgr := NewManager(server.Client(), nil)
	mgr.sendHeartbeat(config.Upstream{
		Name:    "responses-upstream",
		BaseURL: server.URL,
		Token:   "dummy-token",
		APIType: config.APITypeResponses,
	})

	if gotPath != "/v1/responses" {
		t.Fatalf("request path = %q, want %q", gotPath, "/v1/responses")
	}
	if gotUserAgent != upstreammeta.UserAgentCodexCLI {
		t.Fatalf("User-Agent = %q, want %q", gotUserAgent, upstreammeta.UserAgentCodexCLI)
	}
}
