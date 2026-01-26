package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type ModelMapping map[string]string

// APIType represents the type of API protocol
type APIType string

const (
	APITypeAnthropic APIType = "anthropic" // /v1/messages
	APITypeOpenAI    APIType = "openai"    // /v1/chat/completions
	APITypeGemini    APIType = "gemini"    // /v1beta/models/*/generateContent
	APITypeResponses APIType = "responses" // /v1/responses
)

type Upstream struct {
	Name            string       `yaml:"name"`
	BaseURL         string       `yaml:"base_url"`
	Token           string       `yaml:"token"`
	Weight          int          `yaml:"weight"`
	ModelMappings   ModelMapping `yaml:"model_mappings"`
	AvailableModels []string     `yaml:"available_models"`
	MustStream      bool         `yaml:"must_stream"`
	APIType         APIType      `yaml:"api_type"` // "anthropic", "openai", "gemini", or "responses"
}

// GetAPIType returns the API type, defaulting to Anthropic
func (u *Upstream) GetAPIType() APIType {
	if u.APIType == "" {
		return APITypeAnthropic
	}
	return u.APIType
}

func (u *Upstream) MapModel(model string) string {
	if u.ModelMappings == nil {
		return model
	}
	if mapped, ok := u.ModelMappings[model]; ok {
		return mapped
	}
	return model
}

func (u *Upstream) SupportsModel(model string) bool {
	if len(u.AvailableModels) == 0 {
		return true // 未配置则支持所有模型
	}
	for _, m := range u.AvailableModels {
		if m == model {
			return true
		}
	}
	return false
}

type Config struct {
	Bind             string     `yaml:"bind"`
	Listen           string     `yaml:"listen"`
	DefaultMaxTokens int        `yaml:"default_max_tokens"`
	Upstreams        []Upstream `yaml:"upstreams"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}

	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1"
	}

	if cfg.DefaultMaxTokens <= 0 {
		cfg.DefaultMaxTokens = 4096
	}

	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].Weight <= 0 {
			cfg.Upstreams[i].Weight = 1
		}
	}

	return &cfg, nil
}
