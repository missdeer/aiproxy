package proxy

import (
	"testing"

	"github.com/missdeer/aiproxy/config"
)

func TestRefreshAllManagers_NilConfig(t *testing.T) {
	refreshAllManagers(nil)
}

func TestRefreshAllManagers_NoOAuthUpstreams(t *testing.T) {
	cfg := &config.Config{
		UpstreamRequestTimeout: 60,
		Upstreams: []config.Upstream{
			{
				Name:    "test-api",
				BaseURL: "https://api.example.com",
				Weight:  1,
				APIType: config.APITypeAnthropic,
			},
		},
	}
	refreshAllManagers(cfg)
}

func TestRefreshAllManagers_DisabledUpstreamSkipped(t *testing.T) {
	enabled := false
	cfg := &config.Config{
		UpstreamRequestTimeout: 60,
		Upstreams: []config.Upstream{
			{
				Name:      "disabled-codex",
				Enabled:   &enabled,
				Weight:    1,
				APIType:   config.APITypeCodex,
				AuthFiles: []string{"nonexistent.json"},
			},
		},
	}
	refreshAllManagers(cfg)
}

func TestRefreshAllManagers_EmptyAuthFilesSkipped(t *testing.T) {
	cfg := &config.Config{
		UpstreamRequestTimeout: 60,
		Upstreams: []config.Upstream{
			{
				Name:    "no-auth",
				Weight:  1,
				APIType: config.APITypeCodex,
			},
		},
	}
	refreshAllManagers(cfg)
}

func TestAuthManagersRegistry(t *testing.T) {
	oauthTypes := []config.APIType{
		config.APITypeCodex,
		config.APITypeClaudeCode,
		config.APITypeGeminiCLI,
		config.APITypeAntigravity,
		config.APITypeKiro,
	}
	for _, apiType := range oauthTypes {
		if _, ok := authManagers[apiType]; !ok {
			t.Errorf("authManagers missing entry for %q", apiType)
		}
	}
}
