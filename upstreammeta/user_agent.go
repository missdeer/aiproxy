package upstreammeta

import "github.com/missdeer/aiproxy/config"

const (
	UserAgentCodexCLI      = "codex_cli_rs/0.101.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464"
	UserAgentGeminiCLI     = "google-api-nodejs-client/9.15.1"
	UserAgentClaudeCodeCLI = "claude-cli/2.1.50 (external, sdk-cli)"
	UserAgentAntigravity   = "antigravity/1.18.4 Darwin/arm64"
)

// HeartbeatUserAgentForAPIType returns the upstream-specific User-Agent for heartbeat requests.
func HeartbeatUserAgentForAPIType(apiType config.APIType) string {
	switch apiType {
	case config.APITypeResponses:
		// Some /v1/responses gateways validate Codex-style UA.
		return UserAgentCodexCLI
	case config.APITypeOpenAI:
		return UserAgentCodexCLI
	case config.APITypeGemini:
		return UserAgentGeminiCLI
	case config.APITypeAnthropic:
		return UserAgentClaudeCodeCLI
	default:
		return ""
	}
}
