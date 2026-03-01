package config

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

type ModelMapping map[string]string

// APIType represents the type of API protocol
type APIType string

const (
	APITypeAnthropic   APIType = "anthropic"   // /v1/messages
	APITypeOpenAI      APIType = "openai"      // /v1/chat/completions
	APITypeGemini      APIType = "gemini"      // /v1beta/models/*/generateContent
	APITypeResponses   APIType = "responses"   // /v1/responses
	APITypeCodex       APIType = "codex"       // chatgpt.com/backend-api/codex/responses (OAuth)
	APITypeGeminiCLI   APIType = "geminicli"   // cloudcode-pa.googleapis.com/v1internal:streamGenerateContent (OAuth)
	APITypeAntigravity APIType = "antigravity" // daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent (OAuth)
	APITypeClaudeCode  APIType = "claudecode"  // api.anthropic.com/v1/messages (OAuth)
	APITypeKiro        APIType = "kiro"        // codewhisperer/q.amazonaws.com (OAuth, AWS Event Stream)
)

type Upstream struct {
	Name               string       `yaml:"name"`
	Enabled            *bool        `yaml:"enabled"` // Whether this upstream is enabled, defaults to true
	BaseURL            string       `yaml:"base_url"`
	Token              string       `yaml:"token"`
	Weight             int          `yaml:"weight"`
	ModelMappings      ModelMapping `yaml:"model_mappings"`
	AvailableModels    []string     `yaml:"available_models"`
	APIType            APIType      `yaml:"api_type"`            // "anthropic", "openai", "gemini", "responses", or "codex"
	AuthFiles          []string     `yaml:"auth_files"`          // Paths to auth JSON files (used by codex api_type, round-robin)
	RequestCompression string       `yaml:"request_compression"` // zstd (default), gzip, br, none
}

// IsEnabled returns whether this upstream is enabled (defaults to true)
func (u *Upstream) IsEnabled() bool {
	if u.Enabled == nil {
		return true
	}
	return *u.Enabled
}

// GetAPIType returns the API type, defaulting to Anthropic
func (u *Upstream) GetAPIType() APIType {
	if u.APIType == "" {
		return APITypeAnthropic
	}
	return u.APIType
}

// GetAcceptEncoding returns the fixed upstream Accept-Encoding negotiation set.
// This is intentionally not configurable per upstream.
func (u *Upstream) GetAcceptEncoding() string {
	return "gzip, zstd, br, identity"
}

// GetRequestContentEncoding returns normalized request compression encoding.
// Returns "zstd" for empty/unknown (default enabled), or "" for none/identity.
func (u *Upstream) GetRequestContentEncoding() string {
	switch strings.TrimSpace(strings.ToLower(u.RequestCompression)) {
	case "", "zstd", "x-zstd":
		return "zstd"
	case "none", "identity":
		return ""
	case "gzip":
		return "gzip"
	case "x-gzip":
		return "gzip"
	case "br":
		return "br"
	default:
		log.Printf("[CONFIG] upstream %q: unknown request_compression %q, defaulting to zstd", u.Name, u.RequestCompression)
		return "zstd"
	}
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

// NextAuthFile returns the next auth file path using round-robin.
// Safe for concurrent use. Uses upstream name as the key for the counter.
func (u *Upstream) NextAuthFile() string {
	if len(u.AuthFiles) == 0 {
		return ""
	}
	if len(u.AuthFiles) == 1 {
		return u.AuthFiles[0]
	}
	idx := nextAuthFileIndex(u.Name, len(u.AuthFiles))
	return u.AuthFiles[idx]
}

// Global round-robin counters for auth files, keyed by upstream name.
var authFileCounters sync.Map // map[string]*atomic.Uint64

func nextAuthFileIndex(name string, n int) int {
	v, _ := authFileCounters.LoadOrStore(name, &atomic.Uint64{})
	ctr := v.(*atomic.Uint64)
	idx := ctr.Add(1) - 1
	return int(idx % uint64(n))
}

// AuthFileStartIndex returns the next round-robin index for this upstream's auth files.
// Use this as the starting offset when iterating a local snapshot of AuthFiles,
// to spread load without repeatedly calling the global counter.
func (u *Upstream) AuthFileStartIndex() int {
	if len(u.AuthFiles) == 0 {
		return 0
	}
	return nextAuthFileIndex(u.Name, len(u.AuthFiles))
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

// LogConfig configures rotating file logging
type LogConfig struct {
	File       string `yaml:"file"`        // Log file path, empty = stdout
	MaxSize    int    `yaml:"max_size"`    // Max size in MB before rotation (default: 100)
	MaxBackups int    `yaml:"max_backups"` // Max number of old files to keep (default: 3)
	MaxAge     int    `yaml:"max_age"`     // Max days to retain old files (default: 28)
	Compress   bool   `yaml:"compress"`    // Compress rotated files (default: false)
}

type Config struct {
	Bind                   string     `yaml:"bind"`
	Listen                 string     `yaml:"listen"`
	DefaultMaxTokens       int        `yaml:"default_max_tokens"`
	UpstreamRequestTimeout int        `yaml:"upstream_request_timeout"` // Timeout in seconds for upstream requests (default: 60)
	Upstreams              []Upstream `yaml:"upstreams"`
	Log                    LogConfig  `yaml:"log"`
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

	if cfg.UpstreamRequestTimeout <= 0 {
		cfg.UpstreamRequestTimeout = 60 // Default 60 seconds
	}

	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].Weight <= 0 {
			cfg.Upstreams[i].Weight = 1
		}
		if cfg.Upstreams[i].RequestCompression == "" {
			cfg.Upstreams[i].RequestCompression = "zstd"
		}
		// Deduplicate AuthFiles using normalized absolute paths.
		// Relative paths are resolved against the process CWD (not the config file location).
		// On Windows and macOS, comparison is always case-insensitive. This is intentional:
		// the vast majority of these volumes are case-insensitive by default, and the
		// simplicity outweighs the edge case of case-sensitive APFS/NTFS volumes.
		if len(cfg.Upstreams[i].AuthFiles) > 1 {
			caseInsensitive := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
			seen := make(map[string]struct{})
			unique := make([]string, 0, len(cfg.Upstreams[i].AuthFiles))
			for _, f := range cfg.Upstreams[i].AuthFiles {
				key := f
				if abs, err := filepath.Abs(f); err == nil {
					key = abs
				}
				if caseInsensitive {
					key = strings.ToLower(key)
				}
				if _, dup := seen[key]; !dup {
					seen[key] = struct{}{}
					unique = append(unique, f)
				}
			}
			cfg.Upstreams[i].AuthFiles = unique
		}
	}

	// Set log defaults
	if cfg.Log.MaxSize <= 0 {
		cfg.Log.MaxSize = 100 // 100 MB
	}
	if cfg.Log.MaxBackups <= 0 {
		cfg.Log.MaxBackups = 3
	}
	if cfg.Log.MaxAge <= 0 {
		cfg.Log.MaxAge = 28 // 28 days
	}

	return &cfg, nil
}
