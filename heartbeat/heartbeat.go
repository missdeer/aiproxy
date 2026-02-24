package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/missdeer/aiproxy/config"
)

// Manager manages heartbeat goroutines for all upstreams
type Manager struct {
	client    *http.Client
	upstreams []config.Upstream
	cancels   map[string]context.CancelFunc
	mu        sync.RWMutex
}

// NewManager creates a new heartbeat manager
func NewManager(client *http.Client, upstreams []config.Upstream) *Manager {
	return &Manager{
		client:    client,
		upstreams: upstreams,
		cancels:   make(map[string]context.CancelFunc),
	}
}

// Start starts heartbeat goroutines for all enabled upstreams
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, upstream := range m.upstreams {
		if upstream.Heartbeat && upstream.HeartbeatInterval > 0 && upstream.IsEnabled() {
			m.startHeartbeatLocked(upstream)
		}
	}
}

// Stop stops all heartbeat goroutines
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, cancel := range m.cancels {
		log.Printf("[HEARTBEAT] Stopping heartbeat for upstream: %s", name)
		cancel()
		delete(m.cancels, name)
	}
}

// Update updates the heartbeat configuration
func (m *Manager) Update(upstreams []config.Upstream) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop all existing heartbeats
	for name, cancel := range m.cancels {
		cancel()
		delete(m.cancels, name)
	}

	m.upstreams = upstreams

	// Start new heartbeats
	for _, upstream := range m.upstreams {
		if upstream.Heartbeat && upstream.HeartbeatInterval > 0 && upstream.IsEnabled() {
			m.startHeartbeatLocked(upstream)
		}
	}
}

// startHeartbeatLocked starts a heartbeat goroutine for the given upstream
// Must be called with m.mu held
func (m *Manager) startHeartbeatLocked(upstream config.Upstream) {
	// Cancel existing heartbeat if any
	if cancel, ok := m.cancels[upstream.Name]; ok {
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[upstream.Name] = cancel

	log.Printf("[HEARTBEAT] Starting heartbeat for upstream %s (interval: %ds)", upstream.Name, upstream.HeartbeatInterval)

	go m.runHeartbeat(ctx, upstream)
}

// runHeartbeat runs the heartbeat loop for a single upstream
func (m *Manager) runHeartbeat(ctx context.Context, upstream config.Upstream) {
	ticker := time.NewTicker(time.Duration(upstream.HeartbeatInterval) * time.Second)
	defer ticker.Stop()

	// Send initial heartbeat immediately
	m.sendHeartbeat(upstream)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[HEARTBEAT] Heartbeat stopped for upstream: %s", upstream.Name)
			return
		case <-ticker.C:
			m.sendHeartbeat(upstream)
		}
	}
}

// sendHeartbeat sends a minimal heartbeat request to the upstream
func (m *Manager) sendHeartbeat(upstream config.Upstream) {
	apiType := upstream.GetAPIType()
	var url string
	var body []byte
	var err error

	// Select a random model from available models, or use a default
	model := m.selectModel(upstream, apiType)
	if model == "" {
		log.Printf("[HEARTBEAT] No suitable model found for upstream: %s", upstream.Name)
		return
	}

	switch apiType {
	case config.APITypeAnthropic:
		// Anthropic API format
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/messages"
		reqBody := map[string]any{
			"model":      model,
			"max_tokens": 10,
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
		}
		body, err = json.Marshal(reqBody)

	case config.APITypeOpenAI:
		// OpenAI API format
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/chat/completions"
		reqBody := map[string]any{
			"model":      model,
			"max_tokens": 10,
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
		}
		body, err = json.Marshal(reqBody)

	case config.APITypeResponses:
		// Responses API format
		url = strings.TrimSuffix(upstream.BaseURL, "/") + "/v1/responses"
		reqBody := map[string]any{
			"model":             model,
			"max_output_tokens": 10,
			"input":             "hi",
		}
		body, err = json.Marshal(reqBody)

	case config.APITypeGemini:
		// Gemini API format - model is in URL
		url = fmt.Sprintf("%s/v1beta/models/%s:generateContent", strings.TrimSuffix(upstream.BaseURL, "/"), model)
		reqBody := map[string]any{
			"contents": []map[string]any{
				{
					"parts": []map[string]string{
						{"text": "hi"},
					},
				},
			},
			"generationConfig": map[string]int{
				"maxOutputTokens": 10,
			},
		}
		body, err = json.Marshal(reqBody)

	case config.APITypeCodex, config.APITypeGeminiCLI, config.APITypeAntigravity, config.APITypeClaudeCode:
		// OAuth-based APIs - skip heartbeat as they require complex auth flow
		// These APIs auto-refresh tokens on actual requests
		log.Printf("[HEARTBEAT] Skipping heartbeat for OAuth-based upstream: %s (type: %s)", upstream.Name, apiType)
		return

	default:
		log.Printf("[HEARTBEAT] Unsupported API type for heartbeat: %s (upstream: %s)", apiType, upstream.Name)
		return
	}

	if err != nil {
		log.Printf("[HEARTBEAT] Failed to marshal heartbeat request for %s: %v", upstream.Name, err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[HEARTBEAT] Failed to create heartbeat request for %s: %v", upstream.Name, err)
		return
	}

	// Set authentication headers
	switch apiType {
	case config.APITypeAnthropic:
		req.Header.Set("x-api-key", upstream.Token)
		req.Header.Set("anthropic-version", "2023-06-01")
	case config.APITypeOpenAI, config.APITypeResponses:
		req.Header.Set("Authorization", "Bearer "+upstream.Token)
	case config.APITypeGemini:
		if !strings.Contains(url, "key=") {
			url = url + "?key=" + upstream.Token
			req.URL, _ = req.URL.Parse(url)
		}
	}
	req.Header.Set("Content-Type", "application/json")

	// Send request
	resp, err := m.client.Do(req)
	if err != nil {
		log.Printf("[HEARTBEAT] Failed to send heartbeat to %s: %v", upstream.Name, err)
		return
	}
	defer resp.Body.Close()

	// Read and discard response body
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		log.Printf("[HEARTBEAT] Heartbeat to %s (model: %s) returned HTTP %d", upstream.Name, model, resp.StatusCode)
	} else {
		log.Printf("[HEARTBEAT] Heartbeat to %s (model: %s) successful (HTTP %d)", upstream.Name, model, resp.StatusCode)
	}
}

// selectModel selects a model for heartbeat request
// Priority: 1) Random from available_models, 2) Random from model_mappings keys, 3) Default fallback
func (m *Manager) selectModel(upstream config.Upstream, apiType config.APIType) string {
	// Try available_models first
	if len(upstream.AvailableModels) > 0 {
		idx := rand.Intn(len(upstream.AvailableModels))
		clientModel := upstream.AvailableModels[idx]
		// Map to upstream model name if mapping exists
		return upstream.MapModel(clientModel)
	}

	// Try model_mappings keys
	if len(upstream.ModelMappings) > 0 {
		keys := make([]string, 0, len(upstream.ModelMappings))
		for k := range upstream.ModelMappings {
			keys = append(keys, k)
		}
		idx := rand.Intn(len(keys))
		clientModel := keys[idx]
		return upstream.MapModel(clientModel)
	}

	// Fallback to default models based on API type
	switch apiType {
	case config.APITypeAnthropic:
		return "claude-3-haiku-20240307"
	case config.APITypeOpenAI:
		return "gpt-3.5-turbo"
	case config.APITypeResponses:
		return "gpt-4o"
	case config.APITypeGemini:
		return "gemini-1.5-flash"
	default:
		return ""
	}
}
