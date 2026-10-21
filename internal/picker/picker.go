// Package picker chooses which model-server replica serves a request. The
// same pickers back the standalone gateway and the Envoy ext_proc endpoint
// picker.
package picker

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync/atomic"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
)

// Strategy names accepted in configuration.
const (
	StrategyRandom      = "random"
	StrategyRoundRobin  = "round-robin"
	StrategyLeastQueue  = "least-queue"
	StrategyPrefixAware = "prefix-aware"
)

// Strategies lists every supported strategy.
var Strategies = []string{StrategyRandom, StrategyRoundRobin, StrategyLeastQueue, StrategyPrefixAware}

// ValidStrategy reports whether s names a supported strategy.
func ValidStrategy(s string) bool { return slices.Contains(Strategies, s) }

// ErrNoEndpoints is returned when there is nothing to pick from.
var ErrNoEndpoints = errors.New("no endpoints available")

// Request carries what a picker may look at.
type Request struct {
	// Model is the name sent upstream (base model or LoRA adapter). It seeds
	// the prefix hashes because KV caches are not shared across adapters.
	Model string
	// Adapter is the LoRA adapter, or "" for the base model.
	Adapter string
	// Prompt is the rendered prompt used for prefix matching.
	Prompt string
}

// Decision is a picker's choice.
type Decision struct {
	Endpoint *endpoint.Endpoint
	// MatchedBlocks of TotalBlocks prompt blocks were believed to be cached on
	// Endpoint. Only the prefix-aware picker fills these in.
	MatchedBlocks, TotalBlocks int
}

// Picker selects one endpoint out of a non-empty candidate list.
type Picker interface {
	Name() string
	Pick(req *Request, candidates []*endpoint.Endpoint) (Decision, error)
}

// New builds the named picker.
func New(strategy string, prefix PrefixConfig) (Picker, error) {
	switch strategy {
	case StrategyRandom:
		return Random{}, nil
	case StrategyRoundRobin:
		return &RoundRobin{}, nil
	case StrategyLeastQueue:
		return LeastQueue{}, nil
	case StrategyPrefixAware:
		return NewPrefixAware(prefix), nil
	}
	return nil, fmt.Errorf("unknown picker strategy %q", strategy)
}

// Random picks uniformly at random.
type Random struct{}

// Name implements Picker.
func (Random) Name() string { return StrategyRandom }

// Pick implements Picker.
func (Random) Pick(_ *Request, candidates []*endpoint.Endpoint) (Decision, error) {
	if len(candidates) == 0 {
		return Decision{}, ErrNoEndpoints
	}
	return Decision{Endpoint: candidates[rand.IntN(len(candidates))]}, nil
}

// RoundRobin cycles through the candidates.
type RoundRobin struct{ next atomic.Uint64 }

// Name implements Picker.
func (*RoundRobin) Name() string { return StrategyRoundRobin }

// Pick implements Picker.
func (r *RoundRobin) Pick(_ *Request, candidates []*endpoint.Endpoint) (Decision, error) {
	if len(candidates) == 0 {
		return Decision{}, ErrNoEndpoints
	}
	i := r.next.Add(1) - 1
	return Decision{Endpoint: candidates[i%uint64(len(candidates))]}, nil
}

// LeastQueue picks the endpoint with the least outstanding work, breaking
// ties at random.
type LeastQueue struct{}

// Name implements Picker.
func (LeastQueue) Name() string { return StrategyLeastQueue }

// Pick implements Picker.
func (LeastQueue) Pick(_ *Request, candidates []*endpoint.Endpoint) (Decision, error) {
	if len(candidates) == 0 {
		return Decision{}, ErrNoEndpoints
	}
	var best *endpoint.Endpoint
	bestLoad, ties := 0, 0
	for _, ep := range candidates {
		load := ep.Load()
		switch {
		case best == nil || load < bestLoad:
			best, bestLoad, ties = ep, load, 1
		case load == bestLoad:
			ties++
			if rand.IntN(ties) == 0 {
				best = ep
			}
		}
	}
	return Decision{Endpoint: best}, nil
}
