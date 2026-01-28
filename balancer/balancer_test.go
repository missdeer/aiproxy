package balancer

import (
	"testing"

	"github.com/missdeer/aiproxy/config"
)

func TestWeightedRoundRobin_Next(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(true)},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true)},
		{Name: "c", BaseURL: "http://c", Token: "c", Weight: 1, Enabled: boolPtr(true)},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// With equal weights, should cycle through all upstreams
	results := make([]string, 6)
	for i := 0; i < 6; i++ {
		next := wrr.Next()
		results[i] = next.Name
	}

	// Should see each upstream at least once in first 3 calls
	seen := make(map[string]bool)
	for i := 0; i < 3; i++ {
		seen[results[i]] = true
	}
	if len(seen) != 3 {
		t.Errorf("Expected all 3 upstreams in first 3 calls, got %v", results[:3])
	}
}

func TestWeightedRoundRobin_NextForModel(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-x"}},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-y"}},
		{Name: "c", BaseURL: "http://c", Token: "c", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-x"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Should only return a and c, rotating between them
	results := make([]string, 6)
	for i := 0; i < 6; i++ {
		next := wrr.NextForModel("model-x")
		if next == nil {
			t.Fatalf("NextForModel returned nil at iteration %d", i)
		}
		results[i] = next.Name
	}

	// Verify we only see a and c
	for i, name := range results {
		if name != "a" && name != "c" {
			t.Errorf("Expected 'a' or 'c' at position %d, got %q", i, name)
		}
	}

	// Count occurrences - should be roughly equal (3 each in 6 calls)
	counts := make(map[string]int)
	for _, name := range results {
		counts[name]++
	}
	if counts["a"] != 3 || counts["c"] != 3 {
		t.Errorf("Expected 3 each for a and c, got a=%d c=%d", counts["a"], counts["c"])
	}
}

func TestWeightedRoundRobin_NextForModelWithWeights(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 2, Enabled: boolPtr(true), AvailableModels: []string{"model-x"}},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-y"}},
		{Name: "c", BaseURL: "http://c", Token: "c", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-x"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// With weights 2:1 for a:c, in 6 calls we should see a more often than c
	results := make([]string, 6)
	for i := 0; i < 6; i++ {
		next := wrr.NextForModel("model-x")
		if next == nil {
			t.Fatalf("NextForModel returned nil at iteration %d", i)
		}
		results[i] = next.Name
	}

	counts := make(map[string]int)
	for _, name := range results {
		counts[name]++
	}

	// a has weight 2, c has weight 1, so a should appear roughly 2x as often
	if counts["a"] < counts["c"] {
		t.Errorf("Expected 'a' (weight 2) to appear more often than 'c' (weight 1), got a=%d c=%d", counts["a"], counts["c"])
	}
}

func TestWeightedRoundRobin_NextForModelNoMatch(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-x"}},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-y"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	next := wrr.NextForModel("model-z")
	if next != nil {
		t.Errorf("Expected nil when no upstream matches, got %v", next)
	}
}

func TestWeightedRoundRobin_NextForModelWithSkewedWeights(t *testing.T) {
	// Test with skewed weights where low-weight upstream might be skipped
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 10, Enabled: boolPtr(true), AvailableModels: []string{"model-y"}},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-x"}},
		{Name: "c", BaseURL: "http://c", Token: "c", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-x"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Even with highly skewed weights, should find matching upstreams
	for i := 0; i < 20; i++ {
		next := wrr.NextForModel("model-x")
		if next == nil {
			t.Fatalf("NextForModel returned nil at iteration %d", i)
		}
		if next.Name != "b" && next.Name != "c" {
			t.Errorf("Expected 'b' or 'c' at iteration %d, got %q", i, next.Name)
		}
	}
}

func TestWeightedRoundRobin_NextForModelWithUnavailable(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-x"}},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-x"}},
		{Name: "c", BaseURL: "http://c", Token: "c", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"model-x"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Mark 'a' as unavailable by recording failures
	for i := 0; i < 3; i++ {
		wrr.RecordFailure("a")
	}

	// Should only return b and c now
	results := make([]string, 6)
	for i := 0; i < 6; i++ {
		next := wrr.NextForModel("model-x")
		if next == nil {
			t.Fatalf("NextForModel returned nil at iteration %d", i)
		}
		results[i] = next.Name
	}

	for i, name := range results {
		if name == "a" {
			t.Errorf("Expected 'a' to be skipped (unavailable) at position %d", i)
		}
	}
}

func TestWeightedRoundRobin_NextMatching(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(true)},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true)},
		{Name: "c", BaseURL: "http://c", Token: "c", Weight: 1, Enabled: boolPtr(true)},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Only match 'a' and 'c'
	filter := func(u *config.Upstream) bool {
		return u.Name == "a" || u.Name == "c"
	}

	results := make([]string, 6)
	for i := 0; i < 6; i++ {
		next := wrr.NextMatching(filter)
		if next == nil {
			t.Fatalf("NextMatching returned nil at iteration %d", i)
		}
		results[i] = next.Name
	}

	for i, name := range results {
		if name != "a" && name != "c" {
			t.Errorf("Expected 'a' or 'c' at position %d, got %q", i, name)
		}
	}
}

func boolPtr(b bool) *bool {
	return &b
}
