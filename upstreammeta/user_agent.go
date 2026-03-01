package upstreammeta

import "fmt"

const (
	UserAgentCodexCLI      = "codex_cli_rs/0.106.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464"
	UserAgentGeminiCLI     = "google-api-nodejs-client/9.15.1"
	UserAgentClaudeCodeCLI = "claude-cli/2.1.63 (external, sdk-cli)"
	UserAgentAntigravity   = "antigravity/1.19.6 Darwin/arm64"

	kiroVersion = "0.7.45"
)

func KiroUserAgent(machineId string) string {
	if machineId != "" {
		return fmt.Sprintf("aws-sdk-js/1.0.27 ua/2.1 os/linux lang/js md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27 m/E KiroIDE-%s-%s", kiroVersion, machineId)
	}
	return fmt.Sprintf("aws-sdk-js/1.0.27 ua/2.1 os/linux lang/js md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27 m/E KiroIDE-%s", kiroVersion)
}

func KiroAmzUserAgent(machineId string) string {
	if machineId != "" {
		return fmt.Sprintf("aws-sdk-js/1.0.27 KiroIDE %s %s", kiroVersion, machineId)
	}
	return fmt.Sprintf("aws-sdk-js/1.0.27 KiroIDE %s", kiroVersion)
}
