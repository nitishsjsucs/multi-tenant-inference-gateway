package picker

import (
	"container/list"
	"math/rand/v2"
	"sync"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/prefixhash"
)

// PrefixConfig tunes the prefix-aware picker.
type PrefixConfig struct {
	// BlockSize is the number of prompt characters per hashed block. The
	// default of 64 is roughly one 16-token vLLM KV block.
	BlockSize int `yaml:"block_size"`
	// MaxBlocks caps how many leading blocks of a prompt are hashed.
	MaxBlocks int `yaml:"max_blocks"`
	// LRUCapacity is how many block hashes are remembered per endpoint. Size it
	// to the replica's KV-cache capacity in blocks.
	LRUCapacity int `yaml:"lru_capacity"`
	// Weights of the scoring terms. If the block is omitted the defaults apply;
	// if any weight is set, all four are used as given.
	Weights Weights `yaml:"weights"`
	// MaxLoadDelta is the load guard: endpoints with more than this many
	// outstanding requests above the least-loaded candidate are not considered,
	// however good their prefix match.
	MaxLoadDelta int `yaml:"max_load_delta"`
	// MaxKVUtilization excludes endpoints whose KV cache is fuller than this,
	// unless every candidate is.
	MaxKVUtilization float64 `yaml:"max_kv_utilization"`
}

// Weights of the prefix-aware score terms.
type Weights struct {
	Prefix  float64 `yaml:"prefix"`
	Queue   float64 `yaml:"queue"`
	KVCache float64 `yaml:"kv_cache"`
	LoRA    float64 `yaml:"lora"`
}

// DefaultWeights favour cache affinity, then load.
var DefaultWeights = Weights{Prefix: 3, Queue: 2, KVCache: 1, LoRA: 1}

// WithDefaults fills unset fields.
func (c PrefixConfig) WithDefaults() PrefixConfig {
	if c.BlockSize <= 0 {
		c.BlockSize = 64
	}
	if c.MaxBlocks <= 0 {
		c.MaxBlocks = 256
	}
	if c.LRUCapacity <= 0 {
		c.LRUCapacity = 31250
	}
	if c.Weights == (Weights{}) {
		c.Weights = DefaultWeights
	}
	if c.MaxLoadDelta <= 0 {
		c.MaxLoadDelta = 8
	}
	if c.MaxKVUtilization <= 0 {
		c.MaxKVUtilization = 0.95
	}
	return c
}

// PrefixAware routes requests to the replica most likely to hold their
// prompt prefix in its KV cache, subject to a load guard.
//
// It cannot see the servers' caches, so it keeps an approximate per-endpoint
// LRU of the block hashes it has sent to each one. For every eligible
// candidate e:
//
//	score(e) = Wp*prefix(e) + Wq*queue(e) + Wkv*(1-kv(e)) + Wl*lora(e)
//
//	prefix(e) = leading request blocks found in e's index / request blocks
//	queue(e)  = 1 - (load(e) - minLoad) / (MaxLoadDelta + 1)
//	kv(e)     = scraped KV-cache utilisation
//	lora(e)   = 1 adapter active (or base model), 0.5 free slot/unknown, 0 full
//
// Eligible means load(e) <= minLoad + MaxLoadDelta and kv(e) <= MaxKVUtilization.
type PrefixAware struct {
	cfg PrefixConfig

	mu      sync.Mutex
	indexes map[*endpoint.Endpoint]*blockLRU
}

// NewPrefixAware builds a prefix-aware picker.
func NewPrefixAware(cfg PrefixConfig) *PrefixAware {
	return &PrefixAware{cfg: cfg.WithDefaults(), indexes: map[*endpoint.Endpoint]*blockLRU{}}
}

// Name implements Picker.
func (*PrefixAware) Name() string { return StrategyPrefixAware }

// Forget drops the index of an endpoint that left the pool.
func (p *PrefixAware) Forget(ep *endpoint.Endpoint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.indexes, ep)
}

func (p *PrefixAware) index(ep *endpoint.Endpoint) *blockLRU {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.indexes[ep]
	if !ok {
		idx = newBlockLRU(p.cfg.LRUCapacity)
		p.indexes[ep] = idx
	}
	return idx
}

// candidate is an endpoint with the load observed once for this decision.
type candidate struct {
	ep   *endpoint.Endpoint
	load int
	kv   float64
}

// Pick implements Picker.
func (p *PrefixAware) Pick(req *Request, candidates []*endpoint.Endpoint) (Decision, error) {
	if len(candidates) == 0 {
		return Decision{}, ErrNoEndpoints
	}
	hashes := prefixhash.Blocks(req.Model, req.Prompt, p.cfg.BlockSize, p.cfg.MaxBlocks)
	eligible, minLoad := p.guard(candidates)

	var (
		best      candidate
		bestScore float64
		bestMatch int
		ties      int
	)
	const epsilon = 1e-9
	for i, c := range eligible {
		matched := p.index(c.ep).match(hashes)
		score := p.score(req, c, matched, len(hashes), minLoad)
		switch {
		case i == 0 || score > bestScore+epsilon:
			best, bestScore, bestMatch, ties = c, score, matched, 1
		case score > bestScore-epsilon:
			ties++
			if rand.IntN(ties) == 0 {
				best, bestMatch = c, matched
			}
		}
	}
	p.index(best.ep).add(hashes)
	return Decision{Endpoint: best.ep, MatchedBlocks: bestMatch, TotalBlocks: len(hashes)}, nil
}

// guard drops overloaded candidates and returns the minimum load seen.
func (p *PrefixAware) guard(eps []*endpoint.Endpoint) ([]candidate, int) {
	all := make([]candidate, len(eps))
	minLoad := 0
	for i, ep := range eps {
		all[i] = candidate{ep: ep, load: ep.Load(), kv: ep.KVCacheUsage()}
		if i == 0 || all[i].load < minLoad {
			minLoad = all[i].load
		}
	}
	filter := func(checkKV bool) []candidate {
		var out []candidate
		for _, c := range all {
			if c.load <= minLoad+p.cfg.MaxLoadDelta && (!checkKV || c.kv <= p.cfg.MaxKVUtilization) {
				out = append(out, c)
			}
		}
		return out
	}
	if out := filter(true); len(out) > 0 {
		return out, minLoad
	}
	return filter(false), minLoad
}

func (p *PrefixAware) score(req *Request, c candidate, matched, total, minLoad int) float64 {
	w := p.cfg.Weights
	var prefix float64
	if total > 0 {
		prefix = float64(matched) / float64(total)
	}
	queue := 1 - float64(c.load-minLoad)/float64(p.cfg.MaxLoadDelta+1)
	return w.Prefix*prefix + w.Queue*queue + w.KVCache*(1-c.kv) + w.LoRA*loraAffinity(c.ep, req.Adapter)
}

func loraAffinity(ep *endpoint.Endpoint, adapter string) float64 {
	if adapter == "" {
		return 1
	}
	switch ep.Adapter(adapter) {
	case endpoint.AdapterActive:
		return 1
	case endpoint.AdapterFull:
		return 0
	default:
		return 0.5
	}
}

// blockLRU is a bounded set of block hashes with least-recently-used eviction.
type blockLRU struct {
	mu       sync.Mutex
	capacity int
	order    *list.List // front is most recent; values are uint64
	items    map[uint64]*list.Element
}

func newBlockLRU(capacity int) *blockLRU {
	return &blockLRU{capacity: capacity, order: list.New(), items: map[uint64]*list.Element{}}
}

// match returns how many leading hashes are present. Because hashes are
// chained, the first miss ends the shared prefix.
func (l *blockLRU) match(hashes []uint64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, h := range hashes {
		if _, ok := l.items[h]; !ok {
			return i
		}
	}
	return len(hashes)
}

// add marks hashes as recently used. They are inserted last-to-first so the
// earliest blocks are the most recent: eviction trims prompt tails before
// the prefixes that many requests share, as vLLM's block pool does.
func (l *blockLRU) add(hashes []uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(hashes) - 1; i >= 0; i-- {
		h := hashes[i]
		if el, ok := l.items[h]; ok {
			l.order.MoveToFront(el)
			continue
		}
		l.items[h] = l.order.PushFront(h)
	}
	for l.order.Len() > l.capacity {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.items, oldest.Value.(uint64))
	}
}
