package endpoint

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// PoolOptions describes where a pool's replicas live.
type PoolOptions struct {
	Name string
	// URLs is a static list of replica base URLs.
	URLs []string
	// DNS, if set instead of URLs, is a host:port whose A/AAAA records are
	// the replicas (e.g. a Kubernetes headless Service).
	DNS        string
	DNSRefresh time.Duration
	// Lookup overrides the DNS resolver (tests).
	Lookup func(ctx context.Context, host string) ([]string, error)
	Logger *slog.Logger
}

// Pool is the current set of replicas for one base model. The set is
// replaced atomically when DNS discovery observes a change.
type Pool struct {
	opts      PoolOptions
	endpoints atomic.Pointer[[]*Endpoint]

	mu       sync.Mutex
	onRemove []func(*Endpoint)
}

// NewPool builds a pool. Static pools are populated immediately; DNS pools
// are populated by Refresh.
func NewPool(opts PoolOptions) (*Pool, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Lookup == nil {
		opts.Lookup = net.DefaultResolver.LookupHost
	}
	p := &Pool{opts: opts}
	eps := make([]*Endpoint, 0, len(opts.URLs))
	for _, u := range opts.URLs {
		ep, err := New(u)
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", opts.Name, err)
		}
		eps = append(eps, ep)
	}
	p.endpoints.Store(&eps)
	return p, nil
}

// Name returns the pool name.
func (p *Pool) Name() string { return p.opts.Name }

// Endpoints returns every known replica.
func (p *Pool) Endpoints() []*Endpoint { return *p.endpoints.Load() }

// Ready returns the healthy replicas, or all of them if none is healthy
// (routing somewhere beats failing every request on a metrics outage).
func (p *Pool) Ready() []*Endpoint {
	all := p.Endpoints()
	ready := make([]*Endpoint, 0, len(all))
	for _, ep := range all {
		if ep.Healthy() {
			ready = append(ready, ep)
		}
	}
	if len(ready) == 0 {
		return all
	}
	return ready
}

// OnRemove registers a callback for replicas dropped by DNS discovery, so
// per-endpoint state (such as a prefix index) can be released.
func (p *Pool) OnRemove(fn func(*Endpoint)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onRemove = append(p.onRemove, fn)
}

// Refresh re-resolves DNS-discovered pools; it is a no-op for static pools.
// Endpoints whose address is unchanged keep their state.
func (p *Pool) Refresh(ctx context.Context) error {
	if p.opts.DNS == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(p.opts.DNS)
	if err != nil {
		return fmt.Errorf("pool %q: dns %q: %w", p.opts.Name, p.opts.DNS, err)
	}
	ips, err := p.opts.Lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("pool %q: resolve %s: %w", p.opts.Name, host, err)
	}
	slices.Sort(ips)

	current := map[string]*Endpoint{}
	for _, ep := range p.Endpoints() {
		current[ep.Addr] = ep
	}
	next := make([]*Endpoint, 0, len(ips))
	for _, ip := range ips {
		addr := net.JoinHostPort(ip, port)
		if ep, ok := current[addr]; ok {
			next = append(next, ep)
			delete(current, addr)
			continue
		}
		ep, err := New("http://" + addr)
		if err != nil {
			return err
		}
		next = append(next, ep)
	}
	p.endpoints.Store(&next)

	p.mu.Lock()
	callbacks := slices.Clone(p.onRemove)
	p.mu.Unlock()
	for _, gone := range current {
		p.opts.Logger.Info("endpoint removed", "pool", p.opts.Name, "addr", gone.Addr)
		for _, fn := range callbacks {
			fn(gone)
		}
	}
	return nil
}

// Scrape fetches /metrics from every replica concurrently.
func (p *Pool) Scrape(ctx context.Context, client *http.Client) {
	var wg sync.WaitGroup
	for _, ep := range p.Endpoints() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := scrape(ctx, client, ep)
			if err != nil {
				ep.ScrapeFailed()
				p.opts.Logger.Debug("scrape failed", "pool", p.opts.Name, "endpoint", ep.URL, "err", err)
				return
			}
			ep.SetMetrics(m)
		}()
	}
	wg.Wait()
}

// Run scrapes metrics every interval (and refreshes DNS) until ctx ends.
func (p *Pool) Run(ctx context.Context, client *http.Client, interval time.Duration) {
	scrapeTick := time.NewTicker(interval)
	defer scrapeTick.Stop()
	var dnsTick <-chan time.Time
	if p.opts.DNS != "" && p.opts.DNSRefresh > 0 {
		t := time.NewTicker(p.opts.DNSRefresh)
		defer t.Stop()
		dnsTick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-dnsTick:
			if err := p.Refresh(ctx); err != nil {
				p.opts.Logger.Warn("dns refresh failed", "err", err)
			}
		case <-scrapeTick.C:
			scrapeCtx, cancel := context.WithTimeout(ctx, interval)
			p.Scrape(scrapeCtx, client)
			cancel()
		}
	}
}

func scrape(ctx context.Context, client *http.Client, ep *Endpoint) (*Metrics, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return ParseVLLM(resp.Body, time.Now())
}
