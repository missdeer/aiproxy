package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/missdeer/aiproxy/balancer"
	"github.com/missdeer/aiproxy/config"
)

func TestModelsHandler_ReturnsAvailableModels(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:            "upstream-1",
				BaseURL:         "https://api.example.com",
				Tokens:          config.TokenList{"sk-test"},
				APIType:         config.APITypeAnthropic,
				Weight:          1,
				AvailableModels: config.AvailableModelList{"claude-opus", "claude-sonnet"},
			},
			{
				Name:            "upstream-2",
				BaseURL:         "https://api.example2.com",
				Tokens:          config.TokenList{"sk-test2"},
				APIType:         config.APITypeOpenAI,
				Weight:          1,
				AvailableModels: config.AvailableModelList{"gpt-4o", "claude-opus"},
			},
		},
	}

	bal := balancer.NewWeightedRoundRobin(cfg.Upstreams)
	handler := NewModelsHandler(bal)

	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var models []string
	if err := json.NewDecoder(rec.Body).Decode(&models); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should return unique models from all upstreams
	expectedModels := map[string]bool{
		"claude-opus":   true,
		"claude-sonnet": true,
		"gpt-4o":        true,
	}

	if len(models) != len(expectedModels) {
		t.Fatalf("got %d models, want %d", len(models), len(expectedModels))
	}

	for _, model := range models {
		if !expectedModels[model] {
			t.Fatalf("unexpected model: %s", model)
		}
	}
}

func TestModelsHandler_FiltersUnavailableModels(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:            "upstream-1",
				BaseURL:         "https://api.example.com",
				Tokens:          config.TokenList{"sk-test"},
				APIType:         config.APITypeAnthropic,
				Weight:          1,
				AvailableModels: config.AvailableModelList{"claude-opus", "claude-sonnet"},
			},
		},
	}

	bal := balancer.NewWeightedRoundRobin(cfg.Upstreams)

	// Mark claude-opus as unavailable by recording 3 failures
	bal.RecordFailure("upstream-1", "claude-opus")
	bal.RecordFailure("upstream-1", "claude-opus")
	bal.RecordFailure("upstream-1", "claude-opus")

	handler := NewModelsHandler(bal)

	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var models []string
	if err := json.NewDecoder(rec.Body).Decode(&models); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should only return claude-sonnet (claude-opus is circuit-broken)
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}

	if models[0] != "claude-sonnet" {
		t.Fatalf("got model %s, want claude-sonnet", models[0])
	}
}

func TestModelsHandler_EmptyWhenNoUpstreamsConfigured(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{},
	}

	bal := balancer.NewWeightedRoundRobin(cfg.Upstreams)
	handler := NewModelsHandler(bal)

	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var models []string
	if err := json.NewDecoder(rec.Body).Decode(&models); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(models) != 0 {
		t.Fatalf("got %d models, want 0", len(models))
	}
}

func TestModelsHandler_EmptyWhenNoAvailableModelsConfigured(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:    "upstream-1",
				BaseURL: "https://api.example.com",
				Tokens:  config.TokenList{"sk-test"},
				APIType: config.APITypeAnthropic,
				Weight:  1,
				// No AvailableModels configured
			},
		},
	}

	bal := balancer.NewWeightedRoundRobin(cfg.Upstreams)
	handler := NewModelsHandler(bal)

	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var models []string
	if err := json.NewDecoder(rec.Body).Decode(&models); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// When no available_models is configured, upstream supports all models,
	// but we can't enumerate them, so the list should be empty
	if len(models) != 0 {
		t.Fatalf("got %d models, want 0", len(models))
	}
}

func TestModelsHandler_IntegrationWithRequestHandler(t *testing.T) {
	// Create a mock upstream that fails
	failCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCount++
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		UpstreamRequestTimeout: 1,
		Upstreams: []config.Upstream{
			{
				Name:            "test-upstream",
				BaseURL:         upstream.URL,
				Tokens:          config.TokenList{"sk-test"},
				APIType:         config.APITypeAnthropic,
				Weight:          1,
				AvailableModels: config.AvailableModelList{"claude-opus", "claude-sonnet"},
			},
		},
	}

	// Create shared balancer
	bal := balancer.NewWeightedRoundRobin(cfg.Upstreams)

	// Create handlers sharing the same balancer
	anthropicHandler := NewAnthropicHandler(cfg, bal)
	modelsHandler := NewModelsHandler(bal)

	// Initially, both models should be available
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()
	modelsHandler.ServeHTTP(rec, req)

	var models []string
	json.NewDecoder(rec.Body).Decode(&models)
	if len(models) != 2 {
		t.Fatalf("initially got %d models, want 2", len(models))
	}

	// Make 3 failed requests for claude-opus to trigger circuit breaker
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
			"model":"claude-opus",
			"messages":[{"role":"user","content":"hello"}]
		}`))
		rec := httptest.NewRecorder()
		anthropicHandler.ServeHTTP(rec, req)
	}

	// Now check /models - claude-opus should be filtered out
	req = httptest.NewRequest(http.MethodGet, "/models", nil)
	rec = httptest.NewRecorder()
	modelsHandler.ServeHTTP(rec, req)

	models = nil
	json.NewDecoder(rec.Body).Decode(&models)
	if len(models) != 1 {
		t.Fatalf("after circuit break got %d models, want 1", len(models))
	}
	if models[0] != "claude-sonnet" {
		t.Fatalf("got model %s, want claude-sonnet", models[0])
	}

	// Verify the upstream was actually called 3 times
	if failCount != 3 {
		t.Fatalf("upstream called %d times, want 3", failCount)
	}
}

func TestModelsHandler_ConfigReloadUpdatesAvailableModels(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:            "upstream-1",
				BaseURL:         "https://api.example.com",
				Tokens:          config.TokenList{"sk-test"},
				APIType:         config.APITypeAnthropic,
				Weight:          1,
				AvailableModels: config.AvailableModelList{"claude-opus"},
			},
		},
	}

	bal := balancer.NewWeightedRoundRobin(cfg.Upstreams)
	handler := NewModelsHandler(bal)

	// Initially should have claude-opus
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var models []string
	json.NewDecoder(rec.Body).Decode(&models)
	if len(models) != 1 || models[0] != "claude-opus" {
		t.Fatalf("initially got %v, want [claude-opus]", models)
	}

	// Simulate config reload with new models
	newCfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:            "upstream-1",
				BaseURL:         "https://api.example.com",
				Tokens:          config.TokenList{"sk-test"},
				APIType:         config.APITypeAnthropic,
				Weight:          1,
				AvailableModels: config.AvailableModelList{"claude-opus", "claude-sonnet", "gpt-4o"},
			},
		},
	}
	bal.Update(newCfg.Upstreams)

	// Should now have all three models
	req = httptest.NewRequest(http.MethodGet, "/models", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	models = nil
	json.NewDecoder(rec.Body).Decode(&models)
	if len(models) != 3 {
		t.Fatalf("after reload got %d models, want 3", len(models))
	}

	// Verify models are sorted
	expected := []string{"claude-opus", "claude-sonnet", "gpt-4o"}
	for i, model := range models {
		if model != expected[i] {
			t.Fatalf("model[%d] = %s, want %s", i, model, expected[i])
		}
	}
}

func TestModelsHandler_ResponseIsSorted(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:            "upstream-1",
				BaseURL:         "https://api.example.com",
				Tokens:          config.TokenList{"sk-test"},
				APIType:         config.APITypeAnthropic,
				Weight:          1,
				AvailableModels: config.AvailableModelList{"zebra-model", "alpha-model", "beta-model"},
			},
		},
	}

	bal := balancer.NewWeightedRoundRobin(cfg.Upstreams)
	handler := NewModelsHandler(bal)

	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var models []string
	json.NewDecoder(rec.Body).Decode(&models)

	expected := []string{"alpha-model", "beta-model", "zebra-model"}
	if len(models) != len(expected) {
		t.Fatalf("got %d models, want %d", len(models), len(expected))
	}

	for i, model := range models {
		if model != expected[i] {
			t.Fatalf("model[%d] = %s, want %s (not sorted)", i, model, expected[i])
		}
	}
}
