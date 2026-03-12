package config

import (
	"testing"
)

func TestAPIKeyNormalization(t *testing.T) {
	cfg, err := Load("../tmp/test_config.yaml")
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
