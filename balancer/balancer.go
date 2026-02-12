package balancer

import (
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
	states    map[string]*upstreamState
	current   int
	cw        int // current weight for weighted round-robin
	gcd       int
	maxWeight int
	mu        sync.Mutex
}

func NewWeightedRoundRobin(upstreams []config.Upstream) *WeightedRoundRobin {
	if len(upstreams) == 0 {
		return &WeightedRoundRobin{
			states: make(map[string]*upstreamState),
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
			states: make(map[string]*upstreamState),
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

	states := make(map[string]*upstreamState)
	for _, u := range enabledUpstreams {
		states[u.Name] = &upstreamState{}
	}

	return &WeightedRoundRobin{
		upstreams: enabledUpstreams,
		weights:   weights,
		states:    states,
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

// IsAvailable checks if an upstream is available (not in cooldown)
func (w *WeightedRoundRobin) IsAvailable(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.isAvailableLocked(name)
}

// isAvailableLocked checks availability without locking (must be called with lock held)
func (w *WeightedRoundRobin) isAvailableLocked(name string) bool {
	state, ok := w.states[name]
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

// RecordSuccess resets failure count for an upstream
func (w *WeightedRoundRobin) RecordSuccess(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if state, ok := w.states[name]; ok {
		state.failures = 0
	}
}

// RecordFailure records a failure for an upstream
// Returns true if the upstream is now marked as unavailable
func (w *WeightedRoundRobin) RecordFailure(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	state, ok := w.states[name]
	if !ok {
		return false
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
		if next != nil && next.SupportsModel(model) && w.isAvailableLocked(next.Name) {
			return next
		}
	}

	// No match found - restore state to avoid biasing future calls
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
		w.current = -1
		w.gcd = 0
		w.maxWeight = 0
		return
	}

	// Preserve existing state for upstreams that still exist
	newStates := make(map[string]*upstreamState)
	for _, u := range enabledUpstreams {
		if oldState, ok := w.states[u.Name]; ok {
			newStates[u.Name] = oldState
		} else {
			newStates[u.Name] = &upstreamState{}
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
