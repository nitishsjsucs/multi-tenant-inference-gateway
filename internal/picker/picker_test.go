package picker

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
)

func endpoints(t *testing.T, n int) []*endpoint.Endpoint {
	t.Helper()
	eps := make([]*endpoint.Endpoint, n)
	for i := range eps {
		ep, err := endpoint.New(fmt.Sprintf("http://10.0.0.%d:8000", i+1))
		if err != nil {
			t.Fatal(err)
		}
		eps[i] = ep
	}
	return eps
}

func counts(t *testing.T, p Picker, eps []*endpoint.Endpoint, n int, req func(i int) *Request) map[*endpoint.Endpoint]int {
	t.Helper()
	got := map[*endpoint.Endpoint]int{}
	for i := range n {
		d, err := p.Pick(req(i), eps)
		if err != nil {
			t.Fatal(err)
		}
		got[d.Endpoint]++
	}
	return got
}

func plain(int) *Request { return &Request{} }

func TestAllPickersRejectEmpty(t *testing.T) {
	for _, s := range Strategies {
		p, err := New(s, PrefixConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Name() != s {
			t.Errorf("Name() = %s, want %s", p.Name(), s)
		}
		if _, err := p.Pick(&Request{}, nil); !errors.Is(err, ErrNoEndpoints) {
			t.Errorf("%s: err = %v", s, err)
		}
	}
	if _, err := New("magic", PrefixConfig{}); err == nil {
		t.Error("unknown strategy accepted")
	}
}

func TestRandomSpreads(t *testing.T) {
	eps := endpoints(t, 4)
	for ep, n := range counts(t, Random{}, eps, 4000, plain) {
		if n < 800 || n > 1200 {
			t.Errorf("%s got %d of 4000", ep.Addr, n)
		}
	}
}

func TestRoundRobinCycles(t *testing.T) {
	eps := endpoints(t, 3)
	rr := &RoundRobin{}
	for i := range 7 {
		d, _ := rr.Pick(&Request{}, eps)
		if d.Endpoint != eps[i%3] {
			t.Fatalf("pick %d = %s, want %s", i, d.Endpoint.Addr, eps[i%3].Addr)
		}
	}
}

func TestLeastQueue(t *testing.T) {
	eps := endpoints(t, 3)
	eps[0].SetMetrics(&endpoint.Metrics{Waiting: 2, Running: 1})
	eps[2].Acquire()
	eps[2].Acquire()
	// Loads: 3, 0, 2.
	if got := counts(t, LeastQueue{}, eps, 50, plain); got[eps[1]] != 50 {
		t.Errorf("least loaded endpoint not always chosen: %v", got)
	}
	// Ties are broken at random.
	eps[1].Acquire()
	eps[1].Acquire()
	got := counts(t, LeastQueue{}, eps, 400, plain)
	if got[eps[1]] < 100 || got[eps[2]] < 100 || got[eps[0]] != 0 {
		t.Errorf("tie between endpoints 1 and 2 not shared: %v", got)
	}
}

func longPrompt(id string, blocks int) string {
	return strings.Repeat(fmt.Sprintf("%-64s", id), blocks)
}

func TestPrefixAffinity(t *testing.T) {
	eps := endpoints(t, 4)
	p := NewPrefixAware(PrefixConfig{})
	system := longPrompt("shared system prompt", 20)

	first, err := p.Pick(&Request{Model: "m", Prompt: system + "turn one"}, eps)
	if err != nil {
		t.Fatal(err)
	}
	if first.MatchedBlocks != 0 || first.TotalBlocks != 20 {
		t.Errorf("first decision = %d/%d blocks", first.MatchedBlocks, first.TotalBlocks)
	}
	for i := range 20 {
		d, _ := p.Pick(&Request{Model: "m", Prompt: system + longPrompt(fmt.Sprint("turn", i), 2)}, eps)
		if d.Endpoint != first.Endpoint {
			t.Fatalf("request %d sharing the prefix went to %s, want %s", i, d.Endpoint.Addr, first.Endpoint.Addr)
		}
		if d.MatchedBlocks != 20 || d.TotalBlocks != 22 {
			t.Errorf("request %d matched %d/%d blocks", i, d.MatchedBlocks, d.TotalBlocks)
		}
	}
	// The same text under another adapter shares no KV cache.
	d, _ := p.Pick(&Request{Model: "other-adapter", Prompt: system}, eps)
	if d.MatchedBlocks != 0 {
		t.Errorf("different model matched %d blocks", d.MatchedBlocks)
	}
}

func TestPrefixSpreadsUnrelatedPrompts(t *testing.T) {
	eps := endpoints(t, 4)
	p := NewPrefixAware(PrefixConfig{})
	got := counts(t, p, eps, 400, func(i int) *Request {
		return &Request{Model: "m", Prompt: longPrompt(fmt.Sprint("conversation", i), 4)}
	})
	for _, ep := range eps {
		if got[ep] < 60 {
			t.Errorf("%s got only %d of 400 unrelated prompts: %v", ep.Addr, got[ep], got)
		}
	}
}

func TestPrefixLoadGuard(t *testing.T) {
	eps := endpoints(t, 3)
	p := NewPrefixAware(PrefixConfig{MaxLoadDelta: 4})
	prompt := longPrompt("hot prefix", 30)
	hot, _ := p.Pick(&Request{Model: "m", Prompt: prompt}, eps)

	// A little extra load does not break affinity...
	hot.Endpoint.Acquire()
	hot.Endpoint.Acquire()
	if d, _ := p.Pick(&Request{Model: "m", Prompt: prompt}, eps); d.Endpoint != hot.Endpoint {
		t.Errorf("affinity lost at load +2 within the guard band")
	}
	// ...but beyond the guard band the hot endpoint is not eligible.
	for range 3 {
		hot.Endpoint.Acquire()
	}
	d, _ := p.Pick(&Request{Model: "m", Prompt: prompt}, eps)
	if d.Endpoint == hot.Endpoint {
		t.Fatalf("load guard did not divert traffic from an endpoint %d requests above the others", hot.Endpoint.Load())
	}
	if d.MatchedBlocks != 0 {
		t.Errorf("spill-over endpoint should start cold, matched %d", d.MatchedBlocks)
	}
}

func TestPrefixKVGuard(t *testing.T) {
	eps := endpoints(t, 2)
	p := NewPrefixAware(PrefixConfig{})
	prompt := longPrompt("x", 10)
	first, _ := p.Pick(&Request{Model: "m", Prompt: prompt}, eps)
	first.Endpoint.SetMetrics(&endpoint.Metrics{KVCacheUsage: 0.99})
	if d, _ := p.Pick(&Request{Model: "m", Prompt: prompt}, eps); d.Endpoint == first.Endpoint {
		t.Error("endpoint with a full KV cache should be skipped")
	}
	for _, ep := range eps {
		ep.SetMetrics(&endpoint.Metrics{KVCacheUsage: 0.99})
	}
	if _, err := p.Pick(&Request{Model: "m", Prompt: prompt}, eps); err != nil {
		t.Errorf("all endpoints full should still pick one: %v", err)
	}
}

func TestPrefixLoRAAffinity(t *testing.T) {
	eps := endpoints(t, 3)
	active := map[string]struct{}{"support-lora": {}}
	full := map[string]struct{}{"a": {}, "b": {}}
	eps[0].SetMetrics(&endpoint.Metrics{MaxLoRAs: 2, ActiveAdapters: full})
	eps[1].SetMetrics(&endpoint.Metrics{MaxLoRAs: 2, ActiveAdapters: active})
	eps[2].SetMetrics(&endpoint.Metrics{MaxLoRAs: 2, ActiveAdapters: map[string]struct{}{}})
	p := NewPrefixAware(PrefixConfig{})
	got := counts(t, p, eps, 30, func(i int) *Request {
		return &Request{Model: "support-lora", Adapter: "support-lora", Prompt: fmt.Sprint(i)}
	})
	if got[eps[1]] != 30 {
		t.Errorf("requests should go where the adapter is loaded: %v", got)
	}
}

func TestPrefixForget(t *testing.T) {
	eps := endpoints(t, 1)
	p := NewPrefixAware(PrefixConfig{})
	p.Pick(&Request{Model: "m", Prompt: longPrompt("a", 3)}, eps)
	p.Forget(eps[0])
	if d, _ := p.Pick(&Request{Model: "m", Prompt: longPrompt("a", 3)}, eps); d.MatchedBlocks != 0 {
		t.Error("index survived Forget")
	}
}

func TestBlockLRUEvictsTailsFirst(t *testing.T) {
	l := newBlockLRU(3)
	l.add([]uint64{1, 2, 3})
	l.add([]uint64{4})
	if got := l.match([]uint64{1, 2, 3}); got != 2 {
		t.Errorf("match = %d, want 2 (block 3, the oldest tail, evicted)", got)
	}
	if l.order.Len() != 3 {
		t.Errorf("len = %d, want capacity 3", l.order.Len())
	}
	if got := l.match([]uint64{9, 1}); got != 0 {
		t.Errorf("a miss on the first block ends the match, got %d", got)
	}
}
