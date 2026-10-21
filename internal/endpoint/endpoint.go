// Package endpoint tracks model-server replicas: their addresses, the
// requests this process has in flight to them, and the load metrics scraped
// from their Prometheus endpoints.
package endpoint

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
)

// Metrics is a snapshot of a model server's scheduler state, as exported by
// vLLM on /metrics.
type Metrics struct {
	Waiting      int
	Running      int
	KVCacheUsage float64 // fraction in [0, 1]
	// ActiveAdapters holds the LoRA adapters of running and waiting requests.
	ActiveAdapters map[string]struct{}
	// MaxLoRAs is vLLM's --max-loras (0 when not reported).
	MaxLoRAs int
}

// Endpoint is one model-server replica.
type Endpoint struct {
	// URL is the base URL requests are sent to, e.g. http://10.0.0.7:8000.
	URL string
	// Addr is the host:port form used by Envoy's original-destination routing.
	Addr string

	inflight atomic.Int64
	metrics  atomic.Pointer[Metrics]
	failures atomic.Int32
}

// New parses a base URL such as http://10.0.0.7:8000.
func New(rawURL string) (*Endpoint, error) {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("endpoint %q: want an http(s) base URL", rawURL)
	}
	addr := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}
	return &Endpoint{URL: u.String(), Addr: addr}, nil
}

// Acquire records a request routed to the endpoint; call Release when it ends.
func (e *Endpoint) Acquire() { e.inflight.Add(1) }

// Release undoes Acquire.
func (e *Endpoint) Release() { e.inflight.Add(-1) }

// InFlight is the number of requests this process currently has open to e.
func (e *Endpoint) InFlight() int64 { return e.inflight.Load() }

// Metrics returns the last scraped snapshot, or nil if none succeeded yet.
func (e *Endpoint) Metrics() *Metrics { return e.metrics.Load() }

// SetMetrics stores a fresh snapshot and marks the endpoint healthy.
func (e *Endpoint) SetMetrics(m *Metrics) {
	e.metrics.Store(m)
	e.failures.Store(0)
}

// ScrapeFailed records a failed scrape.
func (e *Endpoint) ScrapeFailed() { e.failures.Add(1) }

// unhealthyAfter is the number of consecutive failed scrapes after which an
// endpoint stops receiving traffic (unless every endpoint is unhealthy).
const unhealthyAfter = 3

// Healthy reports whether recent scrapes succeeded. Endpoints that have never
// been scraped are optimistically healthy so traffic can flow at startup.
func (e *Endpoint) Healthy() bool { return e.failures.Load() < unhealthyAfter }

// Load estimates outstanding work on the endpoint: the larger of what this
// process has in flight (exact and immediate) and what the server last
// reported (which also covers traffic from other gateway replicas).
func (e *Endpoint) Load() int {
	load := int(e.inflight.Load())
	if m := e.metrics.Load(); m != nil && m.Waiting+m.Running > load {
		load = m.Waiting + m.Running
	}
	return load
}

// KVCacheUsage returns the last reported KV-cache utilisation (0 if unknown).
func (e *Endpoint) KVCacheUsage() float64 {
	if m := e.metrics.Load(); m != nil {
		return m.KVCacheUsage
	}
	return 0
}

// AdapterState describes whether a LoRA adapter can be served without churn.
type AdapterState int

const (
	// AdapterUnknown means no metrics have been scraped yet.
	AdapterUnknown AdapterState = iota
	// AdapterActive means the adapter is already in the server's batch.
	AdapterActive
	// AdapterLoadable means a free adapter slot exists.
	AdapterLoadable
	// AdapterFull means serving it would need another adapter to drain first.
	AdapterFull
)

// Adapter reports the endpoint's state for the named adapter.
func (e *Endpoint) Adapter(name string) AdapterState {
	m := e.metrics.Load()
	if m == nil {
		return AdapterUnknown
	}
	if _, ok := m.ActiveAdapters[name]; ok {
		return AdapterActive
	}
	if m.MaxLoRAs == 0 || len(m.ActiveAdapters) < m.MaxLoRAs {
		return AdapterLoadable
	}
	return AdapterFull
}
