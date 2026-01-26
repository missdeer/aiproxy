package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type ModelMapping map[string]string

// APIType represents the type of API protocol
type APIType string

const (
	APITypeAnthropic APIType = "anthropic"
	APITypeOpenAI    APIType = "openai"
	APITypeGemini    APIType = "gemini"
)

type Upstream struct {
	Name               string       `yaml:"name"`
	BaseURL            string       `yaml:"base_url"`
	Token              string       `yaml:"token"`
	Weight             int          `yaml:"weight"`
	ModelMappings      ModelMapping `yaml:"model_mappings"`
	AvailableModels    []string     `yaml:"available_models"`
	MustStream         bool         `yaml:"must_stream"`
	APIType            APIType      `yaml:"api_type"`              // "anthropic" or "openai", defaults to "anthropic"
	SupportsResponses  bool         `yaml:"supports_responses"`    // Whether upstream supports /v1/responses natively
}

// GetAPIType returns the API type, defaulting to Anthropic
func (u *Upstream) GetAPIType() APIType {
	if u.APIType == "" {
		return APITypeAnthropic
	}
	return u.APIType
}

// SupportsResponsesAPI returns whether the upstream supports Responses API natively
func (u *Upstream) SupportsResponsesAPI() bool {
	return u.SupportsResponses
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
	Bind      string     `yaml:"bind"`
	Listen    string     `yaml:"listen"`
	Upstreams []Upstream `yaml:"upstreams"`
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

	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].Weight <= 0 {
			cfg.Upstreams[i].Weight = 1
		}
	}

	return &cfg, nil
}
