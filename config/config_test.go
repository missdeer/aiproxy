package config

import (
	"os"
	"path/filepath"
	"runtime"
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
	if cfg.Upstreams[0].Compression != "zstd" {
		t.Errorf("Upstream.Compression default = %q, want %q", cfg.Upstreams[0].Compression, "zstd")
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

func TestUpstreamGetAcceptEncoding(t *testing.T) {
	tests := []struct {
		name        string
		compression string
		want        string
	}{
		{"empty defaults to zstd", "", "zstd"},
		{"zstd", "zstd", "zstd"},
		{"gzip", "gzip", "gzip"},
		{"br", "br", "br"},
		{"none disables header", "none", ""},
		{"unknown falls back to zstd", "snappy", "zstd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := Upstream{Name: "test-upstream", Compression: tt.compression}
			if got := u.GetAcceptEncoding(); got != tt.want {
				t.Errorf("GetAcceptEncoding() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUpstreamGetRequestContentEncoding(t *testing.T) {
	tests := []struct {
		name               string
		requestCompression string
		want               string
	}{
		{"empty defaults to none", "", ""},
		{"none", "none", ""},
		{"identity", "identity", ""},
		{"trim and lowercase", "  GZIP  ", "gzip"},
		{"zstd", "zstd", "zstd"},
		{"br", "br", "br"},
		{"unknown ignored", "snappy", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := Upstream{Name: "test-upstream", RequestCompression: tt.requestCompression}
			if got := u.GetRequestContentEncoding(); got != tt.want {
				t.Errorf("GetRequestContentEncoding() = %q, want %q", got, tt.want)
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

func TestAuthFilesDedup(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("no dedup for single file", func(t *testing.T) {
		configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
    auth_files:
      - "/path/to/auth.json"
`
		configPath := filepath.Join(tmpDir, "single.yaml")
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(cfg.Upstreams[0].AuthFiles); got != 1 {
			t.Errorf("AuthFiles count = %d, want 1", got)
		}
	})

	t.Run("exact duplicates removed", func(t *testing.T) {
		configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
    auth_files:
      - "/path/to/auth.json"
      - "/path/to/other.json"
      - "/path/to/auth.json"
`
		configPath := filepath.Join(tmpDir, "exact_dup.yaml")
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(cfg.Upstreams[0].AuthFiles); got != 2 {
			t.Errorf("AuthFiles count = %d, want 2", got)
		}
		// Order preserved: first occurrence wins
		if cfg.Upstreams[0].AuthFiles[0] != "/path/to/auth.json" {
			t.Errorf("AuthFiles[0] = %q, want %q", cfg.Upstreams[0].AuthFiles[0], "/path/to/auth.json")
		}
		if cfg.Upstreams[0].AuthFiles[1] != "/path/to/other.json" {
			t.Errorf("AuthFiles[1] = %q, want %q", cfg.Upstreams[0].AuthFiles[1], "/path/to/other.json")
		}
	})

	t.Run("relative and absolute duplicates removed", func(t *testing.T) {
		// Create a real file so filepath.Abs works predictably
		authFile := filepath.Join(tmpDir, "auth.json")
		if err := os.WriteFile(authFile, []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
		// Convert to forward slashes for YAML compatibility on Windows
		yamlPath := filepath.ToSlash(authFile)
		configContent := "upstreams:\n  - name: \"test\"\n    base_url: \"https://api.example.com\"\n    token: \"sk-test\"\n    auth_files:\n      - \"" + yamlPath + "\"\n      - \"" + yamlPath + "\"\n"
		configPath := filepath.Join(tmpDir, "rel_abs.yaml")
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(cfg.Upstreams[0].AuthFiles); got != 1 {
			t.Errorf("AuthFiles count = %d, want 1", got)
		}
	})

	t.Run("case variant paths on case-insensitive OS", func(t *testing.T) {
		if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
			t.Skip("case-insensitive dedup only applies on windows/darwin")
		}
		configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
    auth_files:
      - "/Path/To/Auth.json"
      - "/path/to/auth.json"
      - "/path/to/other.json"
`
		configPath := filepath.Join(tmpDir, "case_variant.yaml")
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(cfg.Upstreams[0].AuthFiles); got != 2 {
			t.Errorf("AuthFiles count = %d, want 2", got)
		}
		// First occurrence (original case) preserved
		if cfg.Upstreams[0].AuthFiles[0] != "/Path/To/Auth.json" {
			t.Errorf("AuthFiles[0] = %q, want %q", cfg.Upstreams[0].AuthFiles[0], "/Path/To/Auth.json")
		}
	})

	t.Run("case variant paths preserved on case-sensitive OS", func(t *testing.T) {
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			t.Skip("case-sensitive behavior only applies on linux")
		}
		configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
    auth_files:
      - "/Path/To/Auth.json"
      - "/path/to/auth.json"
`
		configPath := filepath.Join(tmpDir, "case_sensitive.yaml")
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		// On Linux, different cases are different files
		if got := len(cfg.Upstreams[0].AuthFiles); got != 2 {
			t.Errorf("AuthFiles count = %d, want 2", got)
		}
	})

	t.Run("order preserved after dedup", func(t *testing.T) {
		configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
    auth_files:
      - "/c.json"
      - "/a.json"
      - "/b.json"
      - "/a.json"
      - "/c.json"
`
		configPath := filepath.Join(tmpDir, "order.yaml")
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"/c.json", "/a.json", "/b.json"}
		if got := len(cfg.Upstreams[0].AuthFiles); got != len(want) {
			t.Fatalf("AuthFiles count = %d, want %d", got, len(want))
		}
		for i, w := range want {
			if cfg.Upstreams[0].AuthFiles[i] != w {
				t.Errorf("AuthFiles[%d] = %q, want %q", i, cfg.Upstreams[0].AuthFiles[i], w)
			}
		}
	})
}
