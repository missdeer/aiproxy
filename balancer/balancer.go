package balancer

import (
	"sort"
	"sync"
	"time"

	"github.com/missdeer/aiproxy/config"
)

const (
	maxFailures    = 3
	cooldownPeriod = 30 * time.Minute
)

type upstreamState struct {
	failures      int
	unavailable   bool
	unavailableAt time.Time
}

type WeightedRoundRobin struct {
	upstreams []config.Upstream
	weights   []int
	states    map[string]map[string]*upstreamState // upstream name -> model -> state
	current   int
	cw        int // current weight for weighted round-robin
	gcd       int
	maxWeight int
	mu        sync.Mutex
}

func NewWeightedRoundRobin(upstreams []config.Upstream) *WeightedRoundRobin {
	if len(upstreams) == 0 {
		return &WeightedRoundRobin{
			states: make(map[string]map[string]*upstreamState),
		}
	}

	// Filter out disabled upstreams
	var enabledUpstreams []config.Upstream
	for _, u := range upstreams {
		if u.IsEnabled() {
			enabledUpstreams = append(enabledUpstreams, u)
		}
	}

	if len(enabledUpstreams) == 0 {
		return &WeightedRoundRobin{
			states: make(map[string]map[string]*upstreamState),
		}
	}

	weights := make([]int, len(enabledUpstreams))
	maxWeight := 0
	for i, u := range enabledUpstreams {
		w := u.Weight
		if w <= 0 {
			w = 1
		}
		weights[i] = w
		if w > maxWeight {
			maxWeight = w
		}
	}

	gcd := weights[0]
	for _, w := range weights[1:] {
		gcd = gcdFunc(gcd, w)
	}

	return &WeightedRoundRobin{
		upstreams: enabledUpstreams,
		weights:   weights,
		states:    make(map[string]map[string]*upstreamState),
		current:   -1,
		gcd:       gcd,
		maxWeight: maxWeight,
	}
}

func gcdFunc(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// IsAvailable checks if an upstream's model is available (not in cooldown)
func (w *WeightedRoundRobin) IsAvailable(name, model string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.isAvailableLocked(name, model)
}

// isAvailableLocked checks availability without locking (must be called with lock held)
func (w *WeightedRoundRobin) isAvailableLocked(name, model string) bool {
	modelStates, ok := w.states[name]
	if !ok {
		return true
	}

	state, ok := modelStates[model]
	if !ok {
		return true
	}

	if !state.unavailable {
		return true
	}

	// Check if cooldown period has passed
	if time.Since(state.unavailableAt) >= cooldownPeriod {
		state.unavailable = false
		state.failures = 0
		return true
	}

	return false
}

// RecordSuccess resets failure count for an upstream+model pair
func (w *WeightedRoundRobin) RecordSuccess(name, model string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if modelStates, ok := w.states[name]; ok {
		if state, ok := modelStates[model]; ok {
			state.failures = 0
		}
	}
}

// ResetModel immediately clears the circuit-breaker state for an upstream+model pair.
// Returns true if the pair was unavailable and has been reset.
func (w *WeightedRoundRobin) ResetModel(name, model string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if modelStates, ok := w.states[name]; ok {
		if state, ok := modelStates[model]; ok && state.unavailable {
			state.unavailable = false
			state.failures = 0
			return true
		}
	}
	return false
}

// RecordFailure records a failure for an upstream+model pair.
// Returns true if the upstream+model is now marked as unavailable.
func (w *WeightedRoundRobin) RecordFailure(name, model string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	modelStates, ok := w.states[name]
	if !ok {
		modelStates = make(map[string]*upstreamState)
		w.states[name] = modelStates
	}

	state, ok := modelStates[model]
	if !ok {
		state = &upstreamState{}
		modelStates[model] = state
	}

	state.failures++
	if state.failures >= maxFailures {
		state.unavailable = true
		state.unavailableAt = time.Now()
		return true
	}

	return false
}

// Next returns the next upstream using weighted round-robin algorithm
func (w *WeightedRoundRobin) Next() *config.Upstream {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.nextLocked()
}

// nextLocked returns the next upstream (must be called with lock held)
func (w *WeightedRoundRobin) nextLocked() *config.Upstream {
	if len(w.upstreams) == 0 {
		return nil
	}

	if len(w.upstreams) == 1 {
		return &w.upstreams[0]
	}

	for {
		w.current = (w.current + 1) % len(w.upstreams)
		if w.current == 0 {
			w.cw = w.cw - w.gcd
			if w.cw <= 0 {
				w.cw = w.maxWeight
			}
		}
		if w.weights[w.current] >= w.cw {
			return &w.upstreams[w.current]
		}
	}
}

// NextMatching returns the next upstream that matches the given filter function.
// IMPORTANT: The matches function must NOT call any WeightedRoundRobin methods
// (IsAvailable, etc.) as this would cause a deadlock. Use NextForModel instead
// if you need to filter by model and availability.
// It advances the round-robin state until it finds a matching upstream or has
// checked all upstreams. Returns nil if no upstream matches.
func (w *WeightedRoundRobin) NextMatching(matches func(*config.Upstream) bool) *config.Upstream {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.upstreams) == 0 {
		return nil
	}

	// Save state so we can restore on no-match to avoid biasing future calls
	savedCurrent := w.current
	savedCw := w.cw

	// A full WRR output cycle produces exactly sum(weights)/gcd selections.
	totalWeight := 0
	for _, wt := range w.weights {
		totalWeight += wt
	}
	maxIterations := totalWeight / w.gcd

	for i := 0; i < maxIterations; i++ {
		next := w.nextLocked()
		if next != nil && matches(next) {
			return next
		}
	}

	// No match found - restore state to avoid biasing future calls
	w.current = savedCurrent
	w.cw = savedCw

	return nil
}

// NextForModel returns the next available upstream that supports the given model.
// This is the preferred method when filtering by model and availability as it
// avoids deadlock issues with callback functions.
func (w *WeightedRoundRobin) NextForModel(model string) *config.Upstream {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.upstreams) == 0 {
		return nil
	}

	// Save state so we can restore on no-match to avoid biasing future calls
	savedCurrent := w.current
	savedCw := w.cw

	// A full WRR output cycle produces exactly sum(weights)/gcd selections.
	totalWeight := 0
	for _, wt := range w.weights {
		totalWeight += wt
	}
	maxIterations := totalWeight / w.gcd

	for i := 0; i < maxIterations; i++ {
		next := w.nextLocked()
		if next != nil && next.SupportsModel(model) && w.isAvailableLocked(next.Name, model) {
			return next
		}
	}

	// No match found - restore state to avoid biasing future calls
	w.current = savedCurrent
	w.cw = savedCw

	return nil
}

// NextUniqueForModel returns the next available upstream that supports the given
// model and whose name is NOT in the skip set. This handles weighted round-robin
// correctly: with weights like a=5, b=1, c=1 the WRR sequence contains repeated
// entries (a,a,a,a,a,b,c). Plain NextForModel would return "a" twice before "b",
// but NextUniqueForModel skips already-tried names so every unique upstream is
// reached within one full WRR cycle.
//
// If no untried matching upstream exists the internal state is restored (same
// semantics as NextForModel on no-match) and nil is returned.
func (w *WeightedRoundRobin) NextUniqueForModel(model string, skip map[string]bool) *config.Upstream {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.upstreams) == 0 {
		return nil
	}

	savedCurrent := w.current
	savedCw := w.cw

	totalWeight := 0
	for _, wt := range w.weights {
		totalWeight += wt
	}
	maxIterations := totalWeight / w.gcd

	for i := 0; i < maxIterations; i++ {
		next := w.nextLocked()
		if next == nil {
			continue
		}
		if skip[next.Name] {
			continue // skip already-tried, but keep advancing RR
		}
		if next.SupportsModel(model) && w.isAvailableLocked(next.Name, model) {
			return next
		}
	}

	// No untried match found — restore state
	w.current = savedCurrent
	w.cw = savedCw

	return nil
}

// GetAll returns all upstreams in weighted order starting from current position
func (w *WeightedRoundRobin) GetAll() []config.Upstream {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.upstreams) == 0 {
		return nil
	}

	result := make([]config.Upstream, len(w.upstreams))
	copy(result, w.upstreams)
	return result
}

func (w *WeightedRoundRobin) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.upstreams)
}

// Update replaces the upstreams with a new set while preserving circuit breaker state
func (w *WeightedRoundRobin) Update(upstreams []config.Upstream) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Filter out disabled upstreams
	var enabledUpstreams []config.Upstream
	for _, u := range upstreams {
		if u.IsEnabled() {
			enabledUpstreams = append(enabledUpstreams, u)
		}
	}

	if len(enabledUpstreams) == 0 {
		w.upstreams = nil
		w.weights = nil
		w.states = make(map[string]map[string]*upstreamState)
		w.current = -1
		w.gcd = 0
		w.maxWeight = 0
		return
	}

	// Preserve existing state for upstreams that still exist
	enabledNames := make(map[string]bool)
	for _, u := range enabledUpstreams {
		enabledNames[u.Name] = true
	}
	newStates := make(map[string]map[string]*upstreamState)
	for name, modelStates := range w.states {
		if enabledNames[name] {
			newStates[name] = modelStates
		}
	}

	weights := make([]int, len(enabledUpstreams))
	maxWeight := 0
	for i, u := range enabledUpstreams {
		wt := u.Weight
		if wt <= 0 {
			wt = 1
		}
		weights[i] = wt
		if wt > maxWeight {
			maxWeight = wt
		}
	}

	gcd := weights[0]
	for _, wt := range weights[1:] {
		gcd = gcdFunc(gcd, wt)
	}

	w.upstreams = enabledUpstreams
	w.weights = weights
	w.states = newStates
	w.current = -1
	w.cw = 0
	w.gcd = gcd
	w.maxWeight = maxWeight
}

type UnavailableModel struct {
	UpstreamName string        `json:"upstream_name"`
	ModelName    string        `json:"model_name"`
	TimeToReset  time.Duration `json:"-"`
}

// UnavailableModels returns all upstream+model pairs currently in circuit-breaker cooldown.
// Only returns entries for upstreams that are still configured.
func (w *WeightedRoundRobin) UnavailableModels() []UnavailableModel {
	w.mu.Lock()
	defer w.mu.Unlock()

	activeUpstreams := make(map[string]*config.Upstream, len(w.upstreams))
	for i := range w.upstreams {
		activeUpstreams[w.upstreams[i].Name] = &w.upstreams[i]
	}

	var result []UnavailableModel
	now := time.Now()

	for name, modelStates := range w.states {
		u, ok := activeUpstreams[name]
		if !ok {
			continue
		}
		for model, state := range modelStates {
			if len(u.AvailableModels) > 0 && !u.SupportsModel(model) {
				continue
			}
			if !state.unavailable {
				continue
			}
			remaining := cooldownPeriod - now.Sub(state.unavailableAt)
			if remaining <= 0 {
				state.unavailable = false
				state.failures = 0
				continue
			}
			result = append(result, UnavailableModel{
				UpstreamName: name,
				ModelName:    model,
				TimeToReset:  remaining,
			})
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].UpstreamName != result[j].UpstreamName {
			return result[i].UpstreamName < result[j].UpstreamName
		}
		return result[i].ModelName < result[j].ModelName
	})

	return result
}

// AvailableModels returns a sorted list of all models that have at least one available upstream.
// A model is considered available if at least one upstream supports it and is not circuit-broken.
// Note: Only models explicitly listed in upstream.AvailableModels are included.
// Upstreams without AvailableModels configured (supporting all models) are not enumerated.
func (w *WeightedRoundRobin) AvailableModels() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	modelSet := make(map[string]bool)

	for _, u := range w.upstreams {
		if len(u.AvailableModels) == 0 {
			continue
		}
		for _, model := range u.AvailableModels {
			if w.isAvailableLocked(u.Name, model) {
				modelSet[model] = true
			}
		}
	}

	models := make([]string, 0, len(modelSet))
	for model := range modelSet {
		models = append(models, model)
	}

	// Sort for deterministic response order
	sort.Strings(models)
	return models
}
