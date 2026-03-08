package config

import (
	"fmt"
	"log"
	"net/http"
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

// unmarshalStringOrSlice decodes a YAML node that is either a scalar string
// or a sequence of strings. All three list types below share this logic.
func unmarshalStringOrSlice(value *yaml.Node) ([]string, error) {
	switch value.Kind {
	case yaml.ScalarNode:
		switch value.Tag {
		case "", "!!str":
			return []string{value.Value}, nil
		case "!!null":
			return nil, nil
		default:
			return nil, fmt.Errorf("expected a string or a list of strings")
		}
	case yaml.SequenceNode:
		var items []string
		if err := value.Decode(&items); err != nil {
			return nil, err
		}
		return items, nil
	default:
		return nil, fmt.Errorf("expected a string or a list of strings")
	}
}

type TokenList []string

func (t *TokenList) UnmarshalYAML(value *yaml.Node) error {
	v, err := unmarshalStringOrSlice(value)
	*t = v
	return err
}

type AuthFileList []string

func (a *AuthFileList) UnmarshalYAML(value *yaml.Node) error {
	v, err := unmarshalStringOrSlice(value)
	*a = v
	return err
}

type AvailableModelList []string

func (m *AvailableModelList) UnmarshalYAML(value *yaml.Node) error {
	v, err := unmarshalStringOrSlice(value)
	*m = v
	return err
}

type Upstream struct {
	Name               string             `yaml:"name"`
	Enabled            *bool              `yaml:"enabled"`
	BaseURL            string             `yaml:"base_url"`
	Tokens             TokenList          `yaml:"token"`
	Weight             int                `yaml:"weight"`
	ModelMappings      ModelMapping       `yaml:"model_mappings"`
	AvailableModels    AvailableModelList `yaml:"available_models"`
	APIType            APIType            `yaml:"api_type"`
	AuthFiles          AuthFileList       `yaml:"auth_files"`
	RequestCompression string             `yaml:"request_compression"`
	HTTPHeaders        map[string]string  `yaml:"http_headers"`
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

// NextToken returns the next API token using round-robin.
// Safe for concurrent use. Uses upstream name + ":token" as the key.
func (u *Upstream) NextToken() string {
	if len(u.Tokens) == 0 {
		return ""
	}
	if len(u.Tokens) == 1 {
		return u.Tokens[0]
	}
	idx := nextTokenIndex(u.Name, len(u.Tokens))
	return u.Tokens[idx]
}

var tokenCounters sync.Map // map[string]*atomic.Uint64

func nextTokenIndex(name string, n int) int {
	key := name + "\x00token"
	v, _ := tokenCounters.LoadOrStore(key, &atomic.Uint64{})
	ctr := v.(*atomic.Uint64)
	idx := ctr.Add(1) - 1
	return int(idx % uint64(n))
}

// TokenStartIndex returns the next round-robin index for this upstream's tokens.
func (u *Upstream) TokenStartIndex() int {
	if len(u.Tokens) == 0 {
		return 0
	}
	return nextTokenIndex(u.Name, len(u.Tokens))
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
	Bind                   string            `yaml:"bind"`
	Listen                 string            `yaml:"listen"`
	DefaultMaxTokens       int               `yaml:"default_max_tokens"`
	UpstreamRequestTimeout int               `yaml:"upstream_request_timeout"` // Timeout in seconds for upstream requests (default: 60)
	Upstreams              []Upstream        `yaml:"upstreams"`
	Log                    LogConfig         `yaml:"log"`
	ModelFallback          map[string]string `yaml:"model_fallback"` // Model fallback chain: key=model, value=fallback model
}

// GetModelFallback returns the fallback model for the given model, or "" if none configured.
func (c *Config) GetModelFallback(model string) string {
	if c == nil || c.ModelFallback == nil {
		return ""
	}
	return c.ModelFallback[model]
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
		cfg.Upstreams[i].Name = strings.TrimSpace(cfg.Upstreams[i].Name)
		if cfg.Upstreams[i].Weight <= 0 {
			cfg.Upstreams[i].Weight = 1
		}
		if cfg.Upstreams[i].RequestCompression == "" {
			cfg.Upstreams[i].RequestCompression = "zstd"
		}
		// Strip empty/whitespace-only token entries.
		if len(cfg.Upstreams[i].Tokens) > 0 {
			filtered := cfg.Upstreams[i].Tokens[:0]
			for _, t := range cfg.Upstreams[i].Tokens {
				if strings.TrimSpace(t) != "" {
					filtered = append(filtered, t)
				}
			}
			cfg.Upstreams[i].Tokens = filtered
		}
		// Deduplicate Tokens.
		if len(cfg.Upstreams[i].Tokens) > 1 {
			seen := make(map[string]struct{})
			unique := make([]string, 0, len(cfg.Upstreams[i].Tokens))
			for _, t := range cfg.Upstreams[i].Tokens {
				if _, dup := seen[t]; !dup {
					seen[t] = struct{}{}
					unique = append(unique, t)
				}
			}
			cfg.Upstreams[i].Tokens = unique
		}
		// Strip empty/whitespace-only auth_files entries.
		if len(cfg.Upstreams[i].AuthFiles) > 0 {
			filtered := cfg.Upstreams[i].AuthFiles[:0]
			for _, f := range cfg.Upstreams[i].AuthFiles {
				if strings.TrimSpace(f) != "" {
					filtered = append(filtered, f)
				}
			}
			cfg.Upstreams[i].AuthFiles = filtered
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
		if len(cfg.Upstreams[i].HTTPHeaders) > 0 {
			canonical := make(map[string]string, len(cfg.Upstreams[i].HTTPHeaders))
			for k, v := range cfg.Upstreams[i].HTTPHeaders {
				trimmed := strings.TrimSpace(k)
				if trimmed == "" {
					return nil, fmt.Errorf("upstream #%d %q: http_headers contains an empty key", i+1, cfg.Upstreams[i].Name)
				}
				ck := http.CanonicalHeaderKey(trimmed)
				if _, dup := canonical[ck]; dup {
					return nil, fmt.Errorf("upstream #%d %q: duplicate http_headers key %q (case-insensitive)", i+1, cfg.Upstreams[i].Name, k)
				}
				canonical[ck] = v
			}
			cfg.Upstreams[i].HTTPHeaders = canonical
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

	if err := validateUpstreams(cfg.Upstreams); err != nil {
		return nil, err
	}

	if err := validateModelFallback(cfg.ModelFallback); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validAPITypes is the set of recognized api_type values.
var validAPITypes = map[APIType]struct{}{
	APITypeAnthropic:   {},
	APITypeOpenAI:      {},
	APITypeGemini:      {},
	APITypeResponses:   {},
	APITypeCodex:       {},
	APITypeGeminiCLI:   {},
	APITypeAntigravity: {},
	APITypeClaudeCode:  {},
	APITypeKiro:        {},
}

// oauthAPITypes require auth_files instead of base_url + token.
var oauthAPITypes = map[APIType]struct{}{
	APITypeCodex:       {},
	APITypeGeminiCLI:   {},
	APITypeAntigravity: {},
	APITypeClaudeCode:  {},
	APITypeKiro:        {},
}

func validateUpstreams(upstreams []Upstream) error {
	names := make(map[string]int) // normalized name -> 1-based index of first occurrence
	for i := range upstreams {
		u := &upstreams[i]
		idx := i + 1 // 1-based for human-readable messages

		if u.Name == "" {
			return fmt.Errorf("upstream #%d: missing required field \"name\"", idx)
		}

		// Case-insensitive duplicate check (names are already trimmed in Load)
		key := strings.ToLower(u.Name)
		if prev, ok := names[key]; ok {
			return fmt.Errorf("upstream #%d %q: duplicate name (first defined at #%d)", idx, u.Name, prev)
		}
		names[key] = idx

		// Skip further validation for disabled upstreams
		if !u.IsEnabled() {
			continue
		}

		apiType := u.GetAPIType()

		// Validate api_type is recognized
		if u.APIType != "" {
			if _, ok := validAPITypes[u.APIType]; !ok {
				return fmt.Errorf("upstream #%d %q: unknown api_type %q", idx, u.Name, u.APIType)
			}
		}

		if _, oauth := oauthAPITypes[apiType]; oauth {
			// OAuth-based upstreams need auth_files
			if len(u.AuthFiles) == 0 {
				return fmt.Errorf("upstream #%d %q (api_type %q): requires at least one auth_files entry", idx, u.Name, apiType)
			}
		} else {
			// Token-based upstreams need base_url and token
			if strings.TrimSpace(u.BaseURL) == "" {
				return fmt.Errorf("upstream #%d %q (api_type %q): missing required field \"base_url\"", idx, u.Name, apiType)
			}
			if len(u.Tokens) == 0 {
				return fmt.Errorf("upstream #%d %q (api_type %q): missing required field \"token\"", idx, u.Name, apiType)
			}
		}
	}
	return nil
}

func validateModelFallback(chain map[string]string) error {
	for source, target := range chain {
		if strings.TrimSpace(source) == "" {
			return fmt.Errorf("model_fallback contains an empty source model")
		}
		if strings.TrimSpace(target) == "" {
			return fmt.Errorf("model_fallback[%q] points to an empty fallback model", source)
		}
	}
	for start := range chain {
		visited := map[string]struct{}{start: {}}
		current := start
		for {
			next, ok := chain[current]
			if !ok {
				break
			}
			if _, seen := visited[next]; seen {
				return fmt.Errorf("model_fallback contains a cycle: %q -> %q", current, next)
			}
			visited[next] = struct{}{}
			current = next
		}
	}
	return nil
}
