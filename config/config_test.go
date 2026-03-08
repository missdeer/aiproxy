package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	if cfg.Upstreams[0].RequestCompression != "zstd" {
		t.Errorf("Upstream.RequestCompression default = %q, want %q", cfg.Upstreams[0].RequestCompression, "zstd")
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
	u := Upstream{Name: "test-upstream"}
	if got := u.GetAcceptEncoding(); got != "gzip, zstd, br, identity" {
		t.Errorf("GetAcceptEncoding() = %q, want %q", got, "gzip, zstd, br, identity")
	}
}

func TestUpstreamGetRequestContentEncoding(t *testing.T) {
	tests := []struct {
		name               string
		requestCompression string
		want               string
	}{
		{"empty defaults to zstd", "", "zstd"},
		{"none", "none", ""},
		{"identity", "identity", ""},
		{"trim and lowercase", "  GZIP  ", "gzip"},
		{"x-gzip alias", "x-gzip", "gzip"},
		{"zstd", "zstd", "zstd"},
		{"x-zstd alias", "x-zstd", "zstd"},
		{"br", "br", "br"},
		{"unknown defaults to zstd", "snappy", "zstd"},
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

func TestValidateUpstreams(t *testing.T) {
	tmpDir := t.TempDir()

	seq := 0
	loadYAML := func(t *testing.T, content string) (*Config, error) {
		t.Helper()
		seq++
		p := filepath.Join(tmpDir, fmt.Sprintf("test_%d.yaml", seq))
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return Load(p)
	}

	t.Run("missing name", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - base_url: "https://api.example.com"
    token: "sk-test"
`)
		if err == nil || !strings.Contains(err.Error(), "missing required field \"name\"") {
			t.Fatalf("expected missing name error, got %v", err)
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - name: "dup"
    base_url: "https://api.example.com"
    token: "sk-test"
  - name: "dup"
    base_url: "https://api2.example.com"
    token: "sk-test2"
`)
		if err == nil || !strings.Contains(err.Error(), "duplicate name") {
			t.Fatalf("expected duplicate name error, got %v", err)
		}
	})

	t.Run("duplicate name case-insensitive", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - name: "MyUpstream"
    base_url: "https://api.example.com"
    token: "sk-test"
  - name: "myupstream"
    base_url: "https://api2.example.com"
    token: "sk-test2"
`)
		if err == nil || !strings.Contains(err.Error(), "duplicate name") {
			t.Fatalf("expected case-insensitive duplicate name error, got %v", err)
		}
	})

	t.Run("whitespace-only name rejected", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - name: "   "
    base_url: "https://api.example.com"
    token: "sk-test"
`)
		if err == nil || !strings.Contains(err.Error(), "missing required field \"name\"") {
			t.Fatalf("expected missing name error for whitespace-only name, got %v", err)
		}
	})

	t.Run("unknown api_type", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - name: "bad-type"
    base_url: "https://api.example.com"
    token: "sk-test"
    api_type: "opanai"
`)
		if err == nil || !strings.Contains(err.Error(), "unknown api_type") {
			t.Fatalf("expected unknown api_type error, got %v", err)
		}
	})

	t.Run("token-based missing base_url", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - name: "no-url"
    token: "sk-test"
    api_type: "anthropic"
`)
		if err == nil || !strings.Contains(err.Error(), "missing required field \"base_url\"") {
			t.Fatalf("expected missing base_url error, got %v", err)
		}
	})

	t.Run("token-based missing token", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - name: "no-token"
    base_url: "https://api.example.com"
    api_type: "openai"
`)
		if err == nil || !strings.Contains(err.Error(), "missing required field \"token\"") {
			t.Fatalf("expected missing token error, got %v", err)
		}
	})

	t.Run("default api_type requires base_url and token", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - name: "bare"
`)
		if err == nil || !strings.Contains(err.Error(), "missing required field \"base_url\"") {
			t.Fatalf("expected missing base_url error for default api_type, got %v", err)
		}
	})

	for _, apiType := range []string{"codex", "geminicli", "antigravity", "claudecode", "kiro"} {
		t.Run("oauth missing auth_files "+apiType, func(t *testing.T) {
			_, err := loadYAML(t, `
upstreams:
  - name: "oauth-no-auth"
    api_type: "`+apiType+`"
`)
			if err == nil || !strings.Contains(err.Error(), "requires at least one auth_files entry") {
				t.Fatalf("expected auth_files error for %s, got %v", apiType, err)
			}
		})
	}

	t.Run("oauth with empty-string auth_files rejected", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - name: "codex-empty-entry"
    api_type: "codex"
    auth_files:
      - ""
`)
		if err == nil || !strings.Contains(err.Error(), "requires at least one auth_files entry") {
			t.Fatalf("expected auth_files error for empty-string entry, got %v", err)
		}
	})

	t.Run("oauth with whitespace-only auth_files rejected", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - name: "codex-ws-entry"
    api_type: "codex"
    auth_files:
      - "   "
      - ""
`)
		if err == nil || !strings.Contains(err.Error(), "requires at least one auth_files entry") {
			t.Fatalf("expected auth_files error for whitespace-only entries, got %v", err)
		}
	})

	t.Run("auth_files filters blanks keeps valid", func(t *testing.T) {
		cfg, err := loadYAML(t, `
upstreams:
  - name: "codex-mixed"
    api_type: "codex"
    auth_files:
      - ""
      - "/valid/auth.json"
      - "   "
`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := len(cfg.Upstreams[0].AuthFiles); got != 1 {
			t.Fatalf("AuthFiles count = %d, want 1", got)
		}
		if cfg.Upstreams[0].AuthFiles[0] != "/valid/auth.json" {
			t.Errorf("AuthFiles[0] = %q, want %q", cfg.Upstreams[0].AuthFiles[0], "/valid/auth.json")
		}
	})

	t.Run("oauth with auth_files passes", func(t *testing.T) {
		cfg, err := loadYAML(t, `
upstreams:
  - name: "codex-ok"
    api_type: "codex"
    auth_files:
      - "/path/to/auth.json"
`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.Upstreams) != 1 {
			t.Fatalf("expected 1 upstream, got %d", len(cfg.Upstreams))
		}
	})

	t.Run("disabled upstream skips field validation", func(t *testing.T) {
		cfg, err := loadYAML(t, `
upstreams:
  - name: "disabled-bare"
    enabled: false
    api_type: "anthropic"
  - name: "active"
    base_url: "https://api.example.com"
    token: "sk-test"
`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.Upstreams) != 2 {
			t.Fatalf("expected 2 upstreams, got %d", len(cfg.Upstreams))
		}
	})

	t.Run("disabled upstream still checks name and duplicates", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - enabled: false
`)
		if err == nil || !strings.Contains(err.Error(), "missing required field \"name\"") {
			t.Fatalf("expected missing name error even for disabled upstream, got %v", err)
		}
	})

	t.Run("valid mixed upstreams", func(t *testing.T) {
		_, err := loadYAML(t, `
upstreams:
  - name: "anthropic-1"
    base_url: "https://api.example.com"
    token: "sk-test"
    api_type: "anthropic"
  - name: "openai-1"
    base_url: "https://api.openai.com"
    token: "sk-openai"
    api_type: "openai"
  - name: "codex-1"
    api_type: "codex"
    auth_files:
      - "/path/to/auth.json"
  - name: "kiro-1"
    api_type: "kiro"
    auth_files:
      - "/path/to/kiro.json"
`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestLoadHTTPHeaders(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
    http_headers:
      "X-Custom": "value1"
      "Authorization": "Bearer override"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	h := cfg.Upstreams[0].HTTPHeaders
	if len(h) != 2 {
		t.Fatalf("HTTPHeaders count = %d, want 2", len(h))
	}
	if h["X-Custom"] != "value1" {
		t.Errorf("HTTPHeaders[X-Custom] = %q, want %q", h["X-Custom"], "value1")
	}
	if h["Authorization"] != "Bearer override" {
		t.Errorf("HTTPHeaders[Authorization] = %q, want %q", h["Authorization"], "Bearer override")
	}
}

func TestLoadHTTPHeadersEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Upstreams[0].HTTPHeaders != nil {
		t.Errorf("HTTPHeaders = %v, want nil", cfg.Upstreams[0].HTTPHeaders)
	}
}

func TestModelFallback(t *testing.T) {
	t.Run("parses from YAML", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.yaml")
		configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
model_fallback:
  "claude-opus-4-6": "claude-opus-4-5"
  "claude-opus-4-5": "claude-sonnet-4-5"
`
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.ModelFallback) != 2 {
			t.Fatalf("ModelFallback count = %d, want 2", len(cfg.ModelFallback))
		}
		if got := cfg.GetModelFallback("claude-opus-4-6"); got != "claude-opus-4-5" {
			t.Errorf("GetModelFallback(claude-opus-4-6) = %q, want %q", got, "claude-opus-4-5")
		}
		if got := cfg.GetModelFallback("claude-opus-4-5"); got != "claude-sonnet-4-5" {
			t.Errorf("GetModelFallback(claude-opus-4-5) = %q, want %q", got, "claude-sonnet-4-5")
		}
	})

	t.Run("returns empty for unknown model", func(t *testing.T) {
		cfg := &Config{
			ModelFallback: map[string]string{"a": "b"},
		}
		if got := cfg.GetModelFallback("unknown"); got != "" {
			t.Errorf("GetModelFallback(unknown) = %q, want empty", got)
		}
	})

	t.Run("nil map returns empty", func(t *testing.T) {
		cfg := &Config{}
		if got := cfg.GetModelFallback("any"); got != "" {
			t.Errorf("GetModelFallback on nil map = %q, want empty", got)
		}
	})

	t.Run("nil config returns empty", func(t *testing.T) {
		var cfg *Config
		if got := cfg.GetModelFallback("any"); got != "" {
			t.Errorf("GetModelFallback on nil config = %q, want empty", got)
		}
	})

	t.Run("rejects cycle", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.yaml")
		configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
model_fallback:
  "a": "b"
  "b": "a"
`
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(configPath)
		if err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("Load() err = %v, want cycle validation error", err)
		}
	})

	t.Run("rejects empty fallback target", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.yaml")
		configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
model_fallback:
  "a": ""
`
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(configPath)
		if err == nil || !strings.Contains(err.Error(), "empty fallback") {
			t.Fatalf("Load() err = %v, want empty fallback validation error", err)
		}
	})

	t.Run("rejects empty source model", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.yaml")
		configContent := `
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "sk-test"
model_fallback:
  "": "gpt-4o"
`
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(configPath)
		if err == nil || !strings.Contains(err.Error(), "empty source") {
			t.Fatalf("Load() err = %v, want empty source validation error", err)
		}
	})
}
