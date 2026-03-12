package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAPIKeyNormalization(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test_config.yaml")

	configContent := `
bind: "127.0.0.1"
listen: ":8080"
api-key:
  - "valid-key-1"
  - "valid-key-2"
  - ""
  - "valid-key-3"
  - "valid-key-1"
upstreams:
  - name: "test"
    base_url: "https://api.example.com"
    token: "test-token"
    weight: 1
    api_type: "anthropic"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Should have 3 unique non-empty keys
	if len(cfg.APIKeys) != 3 {
		t.Errorf("Expected 3 API keys after normalization, got %d: %v", len(cfg.APIKeys), cfg.APIKeys)
	}

	// Check for expected keys
	expected := map[string]bool{
		"valid-key-1": false,
		"valid-key-2": false,
		"valid-key-3": false,
	}

	for _, key := range cfg.APIKeys {
		if _, ok := expected[key]; ok {
			expected[key] = true
		} else {
			t.Errorf("Unexpected API key: %q", key)
		}
	}

	for key, found := range expected {
		if !found {
			t.Errorf("Expected API key %q not found", key)
		}
	}
}
