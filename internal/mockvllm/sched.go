package mockvllm

import (
	"container/list"
	"context"
	"math"
	"slices"
	"sync"
	"time"
)

// blockCache simulates vLLM's automatic prefix cache: a bounded set of KV
// block hashes with LRU eviction.
type blockCache struct {
	mu       sync.Mutex
	capacity int
	order    *list.List // front = most recently used
	items    map[uint64]*list.Element
}

func newBlockCache(capacity int) *blockCache {
	return &blockCache{capacity: capacity, order: list.New(), items: map[uint64]*list.Element{}}
}

// match returns the number of leading blocks that are cached and marks them used.
func (c *blockCache) match(hashes []uint64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, h := range hashes {
		el, ok := c.items[h]
		if !ok {
			return i
		}
		c.order.MoveToFront(el)
	}
	return len(hashes)
}

// insert caches blocks, evicting the least recently used ones. Blocks go in
// last-to-first so a prompt's tail is evicted before its shared prefix.
func (c *blockCache) insert(hashes []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(hashes) - 1; i >= 0; i-- {
		if el, ok := c.items[hashes[i]]; ok {
			c.order.MoveToFront(el)
			continue
		}
		c.items[hashes[i]] = c.order.PushFront(hashes[i])
	}
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(uint64))
	}
}

// prefillQueue models prefill compute as one FIFO resource per replica:
// each request's uncached prompt tokens occupy it for tokens/rate seconds.
// This is what makes cache misses slow down everyone else on the replica.
type prefillQueue struct {
	mu        sync.Mutex
	rate      float64 // tokens per second
	busyUntil time.Time
}

// schedule reserves prefill time for tokens and returns when it completes.
func (q *prefillQueue) schedule(tokens int) time.Time {
	d := time.Duration(float64(tokens) / q.rate * float64(time.Second))
	q.mu.Lock()
	defer q.mu.Unlock()
	start := time.Now()
	if q.busyUntil.After(start) {
		start = q.busyUntil
	}
	q.busyUntil = start.Add(d)
	return q.busyUntil
}

// loraManager enforces --max-loras: at most max adapters can be resident
// for in-flight requests. A request for a non-resident adapter waits until
// one with no in-flight requests can be evicted, and pays a load penalty.
// Admission is first-come first-served: once a request is blocked on an
// adapter swap, later requests for resident adapters queue behind it, so
// the swap cannot be starved.
type loraManager struct {
	mu       sync.Mutex
	cond     *sync.Cond
	max      int
	loadTime time.Duration
	onLoad   func()

	loaded  map[string]*adapterSlot
	waiters map[uint64]string // ticket -> adapter of not-yet-admitted requests
	next    uint64
}

type adapterSlot struct {
	refs     int
	lastUsed time.Time
}

func newLoRAManager(max int, loadTime time.Duration, onLoad func()) *loraManager {
	m := &loraManager{max: max, loadTime: loadTime, onLoad: onLoad,
		loaded: map[string]*adapterSlot{}, waiters: map[uint64]string{}}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// acquire admits a request for adapter and returns its load penalty.
func (m *loraManager) acquire(ctx context.Context, adapter string) (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ticket := m.next
	m.next++
	m.waiters[ticket] = adapter
	defer func() {
		delete(m.waiters, ticket)
		m.cond.Broadcast() // the head of the queue may have changed
	}()
	stop := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.cond.Broadcast()
	})
	defer stop()

	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if slot, ok := m.loaded[adapter]; ok {
			if m.firstBlocked() > ticket {
				slot.refs++
				return 0, nil
			}
		} else if m.firstBlocked() == ticket && m.makeRoom() {
			m.loaded[adapter] = &adapterSlot{refs: 1}
			m.onLoad()
			return m.loadTime, nil
		}
		m.cond.Wait()
	}
}

// firstBlocked is the earliest ticket waiting for a non-resident adapter.
func (m *loraManager) firstBlocked() uint64 {
	first := uint64(math.MaxUint64)
	for t, a := range m.waiters {
		if _, ok := m.loaded[a]; !ok && t < first {
			first = t
		}
	}
	return first
}

// makeRoom frees a slot by evicting the least recently used idle adapter.
func (m *loraManager) makeRoom() bool {
	if len(m.loaded) < m.max {
		return true
	}
	victim := ""
	for name, s := range m.loaded {
		if s.refs == 0 && (victim == "" || s.lastUsed.Before(m.loaded[victim].lastUsed)) {
			victim = name
		}
	}
	if victim == "" {
		return false
	}
	delete(m.loaded, victim)
	return true
}

func (m *loraManager) release(adapter string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.loaded[adapter]; ok {
		s.refs--
		s.lastUsed = time.Now()
	}
	m.cond.Broadcast()
}

// state reports adapters with running requests and those requests wait for.
func (m *loraManager) state() (running, waiting []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, s := range m.loaded {
		if s.refs > 0 {
			running = append(running, name)
		}
	}
	seen := map[string]bool{}
	for _, a := range m.waiters {
		if !seen[a] {
			seen[a] = true
			waiting = append(waiting, a)
		}
	}
	slices.Sort(running)
	slices.Sort(waiting)
	return running, waiting
}
