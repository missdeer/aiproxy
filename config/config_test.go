package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
bind: "0.0.0.0"
listen: ":9000"
default_max_tokens: 8192
upstreams:
  - name: "test-upstream"
    base_url: "https://api.example.com"
    token: "sk-test"
    weight: 10
    api_type: "anthropic"
    model_mappings:
      "gpt-4": "claude-3"
    available_models:
      - "gpt-4"
      - "gpt-3.5"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Bind != "0.0.0.0" {
		t.Errorf("Bind = %q, want %q", cfg.Bind, "0.0.0.0")
	}
	if cfg.Listen != ":9000" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":9000")
	}
	if cfg.DefaultMaxTokens != 8192 {
		t.Errorf("DefaultMaxTokens = %d, want %d", cfg.DefaultMaxTokens, 8192)
	}
	if len(cfg.Upstreams) != 1 {
		t.Fatalf("Upstreams count = %d, want 1", len(cfg.Upstreams))
	}

	upstream := cfg.Upstreams[0]
	if upstream.Name != "test-upstream" {
		t.Errorf("Upstream.Name = %q, want %q", upstream.Name, "test-upstream")
	}
	if upstream.Weight != 10 {
		t.Errorf("Upstream.Weight = %d, want %d", upstream.Weight, 10)
	}
	if upstream.APIType != APITypeAnthropic {
		t.Errorf("Upstream.APIType = %q, want %q", upstream.APIType, APITypeAnthropic)
	}
}

func TestLoadDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Minimal config without optional fields
	configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Check defaults
	if cfg.Bind != "127.0.0.1" {
		t.Errorf("Bind default = %q, want %q", cfg.Bind, "127.0.0.1")
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen default = %q, want %q", cfg.Listen, ":8080")
	}
	if cfg.DefaultMaxTokens != 4096 {
		t.Errorf("DefaultMaxTokens default = %d, want %d", cfg.DefaultMaxTokens, 4096)
	}
	if cfg.Upstreams[0].Weight != 1 {
		t.Errorf("Upstream.Weight default = %d, want %d", cfg.Upstreams[0].Weight, 1)
	}
}

func TestUpstreamMapModel(t *testing.T) {
	upstream := Upstream{
		ModelMappings: ModelMapping{
			"gpt-4":   "claude-3-opus",
			"gpt-3.5": "claude-3-haiku",
		},
	}

	tests := []struct {
		input string
		want  string
	}{
		{"gpt-4", "claude-3-opus"},
		{"gpt-3.5", "claude-3-haiku"},
		{"unknown-model", "unknown-model"}, // Should return original if no mapping
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := upstream.MapModel(tt.input); got != tt.want {
				t.Errorf("MapModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestUpstreamMapModelNilMappings(t *testing.T) {
	upstream := Upstream{}
	model := "gpt-4"

	if got := upstream.MapModel(model); got != model {
		t.Errorf("MapModel(%q) with nil mappings = %q, want %q", model, got, model)
	}
}

func TestUpstreamSupportsModel(t *testing.T) {
	tests := []struct {
		name            string
		availableModels []string
		model           string
		want            bool
	}{
		{
			name:            "empty available_models supports all",
			availableModels: nil,
			model:           "any-model",
			want:            true,
		},
		{
			name:            "model in list",
			availableModels: []string{"gpt-4", "gpt-3.5"},
			model:           "gpt-4",
			want:            true,
		},
		{
			name:            "model not in list",
			availableModels: []string{"gpt-4", "gpt-3.5"},
			model:           "claude-3",
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := Upstream{AvailableModels: tt.availableModels}
			if got := upstream.SupportsModel(tt.model); got != tt.want {
				t.Errorf("SupportsModel(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestUpstreamGetAPIType(t *testing.T) {
	tests := []struct {
		name    string
		apiType APIType
		want    APIType
	}{
		{"empty defaults to anthropic", "", APITypeAnthropic},
		{"explicit anthropic", APITypeAnthropic, APITypeAnthropic},
		{"explicit openai", APITypeOpenAI, APITypeOpenAI},
		{"explicit gemini", APITypeGemini, APITypeGemini},
		{"explicit responses", APITypeResponses, APITypeResponses},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := Upstream{APIType: tt.apiType}
			if got := upstream.GetAPIType(); got != tt.want {
				t.Errorf("GetAPIType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUpstreamIsEnabled(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name    string
		enabled *bool
		want    bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", &trueVal, true},
		{"explicit false", &falseVal, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := Upstream{Enabled: tt.enabled}
			if got := upstream.IsEnabled(); got != tt.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
