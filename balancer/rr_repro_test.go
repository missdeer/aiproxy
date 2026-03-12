package balancer

import (
	"testing"

	"github.com/missdeer/aiproxy/config"
)

// TestRoundRobinAdvancesWithFailures verifies the key behavior:
// when request 1 tries a(fail), b(fail), c(success), the next request starts from d.
func TestRoundRobinAdvancesWithFailures(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Tokens: config.TokenList{"a"}, Weight: 1, Enabled: boolPtr(true), AvailableModels: config.AvailableModelList{"model-x"}},
		{Name: "b", BaseURL: "http://b", Tokens: config.TokenList{"b"}, Weight: 1, Enabled: boolPtr(true), AvailableModels: config.AvailableModelList{"model-x"}},
		{Name: "c", BaseURL: "http://c", Tokens: config.TokenList{"c"}, Weight: 1, Enabled: boolPtr(true), AvailableModels: config.AvailableModelList{"model-x"}},
		{Name: "d", BaseURL: "http://d", Tokens: config.TokenList{"d"}, Weight: 1, Enabled: boolPtr(true), AvailableModels: config.AvailableModelList{"model-x"}},
		{Name: "e", BaseURL: "http://e", Tokens: config.TokenList{"e"}, Weight: 1, Enabled: boolPtr(true), AvailableModels: config.AvailableModelList{"model-x"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Simulate the handler pattern: call NextUniqueForModel for each attempt.
	simulateRequest := func(failNames map[string]bool) string {
		tried := make(map[string]bool)
		for {
			next := wrr.NextUniqueForModel("model-x", tried)
			if next == nil {
				return "" // exhausted all
			}
			tried[next.Name] = true
			if !failNames[next.Name] {
				return next.Name // success
			}
		}
	}

	// Request 1: a and b fail, c succeeds
	result := simulateRequest(map[string]bool{"a": true, "b": true})
	if result != "c" {
		t.Errorf("Request 1: expected c, got %s", result)
	}

	// Request 2: should start from d (not from a!)
	result = simulateRequest(map[string]bool{})
	if result != "d" {
		t.Errorf("Request 2: expected d (round-robin should advance past tried upstreams), got %s", result)
	}

	// Request 3: should start from e
	result = simulateRequest(map[string]bool{})
	if result != "e" {
		t.Errorf("Request 3: expected e, got %s", result)
	}

	// Request 4: should wrap to a
	result = simulateRequest(map[string]bool{})
	if result != "a" {
		t.Errorf("Request 4: expected a (wrap around), got %s", result)
	}
}

// TestRoundRobinWeightedFailover verifies that with unequal weights (a=5, b=1, c=1),
// if 'a' fails, the handler still reaches b and c.
func TestRoundRobinWeightedFailover(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Tokens: config.TokenList{"a"}, Weight: 5, Enabled: boolPtr(true)},
		{Name: "b", BaseURL: "http://b", Tokens: config.TokenList{"b"}, Weight: 1, Enabled: boolPtr(true)},
		{Name: "c", BaseURL: "http://c", Tokens: config.TokenList{"c"}, Weight: 1, Enabled: boolPtr(true)},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Simulate: 'a' always fails, should still reach b and c
	tried := make(map[string]bool)
	var sequence []string

	for {
		next := wrr.NextUniqueForModel("model-x", tried)
		if next == nil {
			break
		}
		tried[next.Name] = true
		sequence = append(sequence, next.Name)
	}

	// Should have tried all 3 unique upstreams: a, b, c (in WRR priority order)
	if len(sequence) != 3 {
		t.Fatalf("expected 3 unique upstreams, got %d: %v", len(sequence), sequence)
	}

	// First should be 'a' (highest weight)
	if sequence[0] != "a" {
		t.Errorf("expected first upstream to be 'a', got %s", sequence[0])
	}

	// b and c should both appear
	found := map[string]bool{}
	for _, name := range sequence {
		found[name] = true
	}
	if !found["b"] || !found["c"] {
		t.Errorf("expected b and c to both appear, got sequence: %v", sequence)
	}
}

// TestRoundRobinSingleSuccessAdvancesOne verifies that a single successful
// upstream advances the RR by exactly 1 slot.
func TestRoundRobinSingleSuccessAdvancesOne(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Tokens: config.TokenList{"a"}, Weight: 1, Enabled: boolPtr(true)},
		{Name: "b", BaseURL: "http://b", Tokens: config.TokenList{"b"}, Weight: 1, Enabled: boolPtr(true)},
		{Name: "c", BaseURL: "http://c", Tokens: config.TokenList{"c"}, Weight: 1, Enabled: boolPtr(true)},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Each request succeeds immediately — should cycle a, b, c, a, b, c, ...
	expected := []string{"a", "b", "c", "a", "b", "c"}
	for i, exp := range expected {
		tried := make(map[string]bool)
		next := wrr.NextUniqueForModel("model-x", tried)
		if next == nil {
			t.Fatalf("Call %d: expected %s, got nil", i+1, exp)
		}
		if next.Name != exp {
			t.Errorf("Call %d: expected %s, got %s", i+1, exp, next.Name)
		}
	}
}

// TestRoundRobinWeightedCircuitBreakerSkip verifies the combination of:
// - unequal weights (a=5, b=1, c=1)
// - circuit breaker marking 'a' unavailable (via 3 consecutive failures)
// - NextUniqueForModel skipping 'a' and its repeated WRR slots
// After 'a' is tripped, the caller should still reach b and c.
func TestRoundRobinWeightedCircuitBreakerSkip(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Tokens: config.TokenList{"a"}, Weight: 5, Enabled: boolPtr(true)},
		{Name: "b", BaseURL: "http://b", Tokens: config.TokenList{"b"}, Weight: 1, Enabled: boolPtr(true)},
		{Name: "c", BaseURL: "http://c", Tokens: config.TokenList{"c"}, Weight: 1, Enabled: boolPtr(true)},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Trip the circuit breaker on 'a' for model-x (3 consecutive failures)
	wrr.RecordFailure("a", "model-x")
	wrr.RecordFailure("a", "model-x")
	tripped := wrr.RecordFailure("a", "model-x")
	if !tripped {
		t.Fatal("expected circuit breaker to trip after 3 failures")
	}

	// Now 'a' is unavailable. NextUniqueForModel should skip all of a's WRR slots
	// and return b and c.
	tried := make(map[string]bool)
	var sequence []string

	for {
		next := wrr.NextUniqueForModel("model-x", tried)
		if next == nil {
			break
		}
		tried[next.Name] = true
		sequence = append(sequence, next.Name)
	}

	// 'a' should NOT appear (circuit breaker open), only b and c
	if len(sequence) != 2 {
		t.Fatalf("expected 2 upstreams (b and c), got %d: %v", len(sequence), sequence)
	}

	found := map[string]bool{}
	for _, name := range sequence {
		found[name] = true
	}
	if found["a"] {
		t.Errorf("'a' should be unavailable but appeared in sequence: %v", sequence)
	}
	if !found["b"] || !found["c"] {
		t.Errorf("expected b and c, got sequence: %v", sequence)
	}

	// After circuit breaker, the next request should still work (b or c)
	tried2 := make(map[string]bool)
	next := wrr.NextUniqueForModel("model-x", tried2)
	if next == nil {
		t.Fatal("expected a non-nil upstream after circuit breaker skip")
	}
	if next.Name == "a" {
		t.Errorf("'a' still unavailable, should not be returned, got: %s", next.Name)
	}
}
