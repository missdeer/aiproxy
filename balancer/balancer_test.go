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

	// Mark 'a' as unavailable for model-x by recording failures
	for i := 0; i < 3; i++ {
		wrr.RecordFailure("a", "model-x")
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

func TestWeightedRoundRobin_NextMatchingNoMatchDoesNotBias(t *testing.T) {
	// Regression: interleaving no-match NextMatching calls should not bias
	// subsequent matching calls. Mirrors NextForModelNoMatchDoesNotBias.
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 10, Enabled: boolPtr(true)},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true)},
		{Name: "c", BaseURL: "http://c", Token: "c", Weight: 1, Enabled: boolPtr(true)},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	neverMatch := func(u *config.Upstream) bool { return false }
	matchBC := func(u *config.Upstream) bool { return u.Name == "b" || u.Name == "c" }

	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		// Interleave a no-match call
		wrr.NextMatching(neverMatch)
		// Then do the real call
		next := wrr.NextMatching(matchBC)
		if next == nil {
			t.Fatalf("NextMatching returned nil at iteration %d", i)
		}
		counts[next.Name]++
	}

	// Deterministic WRR with equal weights must yield exactly 50/50.
	if counts["b"] != 50 || counts["c"] != 50 {
		t.Errorf("Expected b=50 c=50, got b=%d c=%d", counts["b"], counts["c"])
	}
}

func TestWeightedRoundRobin_NextForModelNoMatchDoesNotBias(t *testing.T) {
	// Regression: interleaving no-match calls should not bias filtered routing.
	// Repro: weights 10:1:1, model m2 only on b/c.
	// Without fix, interleaving NextForModel("none") before each NextForModel("m2")
	// would result in b:100 c:0 instead of b:50 c:50.
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 10, Enabled: boolPtr(true), AvailableModels: []string{"m1"}},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"m2"}},
		{Name: "c", BaseURL: "http://c", Token: "c", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"m2"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		// Interleave a no-match call
		wrr.NextForModel("none")
		// Then do the real call
		next := wrr.NextForModel("m2")
		if next == nil {
			t.Fatalf("NextForModel returned nil at iteration %d", i)
		}
		counts[next.Name]++
	}

	// Deterministic WRR with equal weights must yield exactly 50/50.
	if counts["b"] != 50 || counts["c"] != 50 {
		t.Errorf("Expected b=50 c=50, got b=%d c=%d", counts["b"], counts["c"])
	}
}

func TestWeightedRoundRobin_ZeroWeightNoPanic(t *testing.T) {
	// Regression: zero-weight upstream should not cause division-by-zero panic.
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 0, Enabled: boolPtr(true), AvailableModels: []string{"m1"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	next := wrr.Next()
	if next == nil {
		t.Fatal("Expected non-nil upstream")
	}
	if next.Name != "a" {
		t.Errorf("Expected upstream 'a', got %q", next.Name)
	}

	// Also test NextForModel with zero weight
	next = wrr.NextForModel("m1")
	if next == nil {
		t.Fatal("NextForModel returned nil for zero-weight upstream")
	}

	// Test no-match path doesn't panic either
	next = wrr.NextForModel("none")
	if next != nil {
		t.Errorf("Expected nil for non-matching model, got %v", next)
	}
}

func TestWeightedRoundRobin_UpdateZeroWeightNoPanic(t *testing.T) {
	// Regression: Update with zero-weight upstream should not cause panic.
	wrr := NewWeightedRoundRobin([]config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(true)},
	})

	wrr.Update([]config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 0, Enabled: boolPtr(true)},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 0, Enabled: boolPtr(true)},
	})

	// Should not panic, and should return upstreams (treated as weight 1)
	for i := 0; i < 4; i++ {
		next := wrr.Next()
		if next == nil {
			t.Fatalf("Next returned nil at iteration %d", i)
		}
	}
}

func TestWeightedRoundRobin_UpdatePreservesPerModelState(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"m1", "m2"}},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"m1", "m2"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Mark 'a' model 'm1' as unavailable
	for i := 0; i < 3; i++ {
		wrr.RecordFailure("a", "m1")
	}

	// Verify 'a' is unavailable for m1 but available for m2
	if wrr.IsAvailable("a", "m1") {
		t.Error("Expected 'a' model 'm1' to be unavailable")
	}
	if !wrr.IsAvailable("a", "m2") {
		t.Error("Expected 'a' model 'm2' to still be available")
	}

	// Update with same upstreams — state should be preserved
	wrr.Update(upstreams)

	if wrr.IsAvailable("a", "m1") {
		t.Error("Expected 'a' model 'm1' to remain unavailable after Update")
	}
	if !wrr.IsAvailable("a", "m2") {
		t.Error("Expected 'a' model 'm2' to remain available after Update")
	}
	if !wrr.IsAvailable("b", "m1") {
		t.Error("Expected 'b' model 'm1' to remain available after Update")
	}
}

func TestWeightedRoundRobin_UpdateRemovesStateForRemovedUpstream(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"m1"}},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"m1"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Mark both upstreams as unavailable for m1
	for i := 0; i < 3; i++ {
		wrr.RecordFailure("a", "m1")
		wrr.RecordFailure("b", "m1")
	}

	// Update removing 'a', keeping 'b'
	wrr.Update([]config.Upstream{
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"m1"}},
	})

	// 'b' state should be preserved (still unavailable)
	if wrr.IsAvailable("b", "m1") {
		t.Error("Expected 'b' model 'm1' to remain unavailable after Update")
	}

	// Re-add 'a' — should start fresh (available)
	wrr.Update([]config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"m1"}},
		{Name: "b", BaseURL: "http://b", Token: "b", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"m1"}},
	})

	if !wrr.IsAvailable("a", "m1") {
		t.Error("Expected re-added 'a' model 'm1' to be available")
	}
	if wrr.IsAvailable("b", "m1") {
		t.Error("Expected 'b' model 'm1' to remain unavailable")
	}
}

func TestWeightedRoundRobin_UpdateClearsStateWhenAllDisabled(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(true), AvailableModels: []string{"m1"}},
	}

	wrr := NewWeightedRoundRobin(upstreams)

	// Mark 'a' as unavailable for m1
	for i := 0; i < 3; i++ {
		wrr.RecordFailure("a", "m1")
	}

	// Update with all disabled — should clear states
	wrr.Update([]config.Upstream{
		{Name: "a", BaseURL: "http://a", Token: "a", Weight: 1, Enabled: boolPtr(false), AvailableModels: []string{"m1"}},
	})

	// Re-enable 'a' — should start fresh
	wrr.Update(upstreams)

	if !wrr.IsAvailable("a", "m1") {
		t.Error("Expected 'a' model 'm1' to be available after re-enable")
	}
}

func boolPtr(b bool) *bool {
	return &b
}
