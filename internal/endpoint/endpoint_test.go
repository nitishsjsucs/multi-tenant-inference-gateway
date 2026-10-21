package endpoint

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Excerpt in the format vLLM's v1 engine exports.
const vllmV1 = `# HELP vllm:num_requests_running Number of requests in model execution batches.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="meta-llama/Llama-3.1-8B-Instruct"} 7.0
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0",model_name="meta-llama/Llama-3.1-8B-Instruct"} 3.0
# HELP vllm:kv_cache_usage_perc KV-cache usage. 1 means 100 percent usage.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0",model_name="meta-llama/Llama-3.1-8B-Instruct"} 0.42
# HELP vllm:lora_requests_info Running stats on lora requests.
# TYPE vllm:lora_requests_info gauge
vllm:lora_requests_info{max_lora="2",running_lora_adapters="sql-lora-v1",waiting_lora_adapters=""} 1.7295e+09
vllm:lora_requests_info{max_lora="2",running_lora_adapters="support-lora-v2, sql-lora-v1",waiting_lora_adapters="legal-lora-v1"} 1.7296e+09
# HELP vllm:prefix_cache_queries_total Prefix cache queries, in terms of number of queried tokens.
# TYPE vllm:prefix_cache_queries_total counter
vllm:prefix_cache_queries_total{engine="0",model_name="meta-llama/Llama-3.1-8B-Instruct"} 1000.0
`

func TestParseVLLMv1(t *testing.T) {
	m, err := ParseVLLM(strings.NewReader(vllmV1))
	if err != nil {
		t.Fatal(err)
	}
	if m.Running != 7 || m.Waiting != 3 || m.KVCacheUsage != 0.42 || m.MaxLoRAs != 2 {
		t.Errorf("parsed %+v", m)
	}
	// The newest lora_requests_info series wins; running and waiting are merged.
	for _, a := range []string{"support-lora-v2", "sql-lora-v1", "legal-lora-v1"} {
		if _, ok := m.ActiveAdapters[a]; !ok {
			t.Errorf("adapter %s missing from %v", a, m.ActiveAdapters)
		}
	}
}

func TestParseVLLMv0CacheMetric(t *testing.T) {
	const v0 = "# TYPE vllm:gpu_cache_usage_perc gauge\nvllm:gpu_cache_usage_perc{model_name=\"m\"} 0.9\n"
	m, err := ParseVLLM(strings.NewReader(v0))
	if err != nil {
		t.Fatal(err)
	}
	if m.KVCacheUsage != 0.9 || len(m.ActiveAdapters) != 0 {
		t.Errorf("parsed %+v", m)
	}
}

func TestEndpointLoadAndAdapterState(t *testing.T) {
	ep, err := New("http://10.0.0.1:8000/")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Addr != "10.0.0.1:8000" || ep.URL != "http://10.0.0.1:8000" {
		t.Errorf("addr=%s url=%s", ep.Addr, ep.URL)
	}
	if ep.Adapter("x") != AdapterUnknown {
		t.Error("no metrics yet: adapter state should be unknown")
	}
	ep.Acquire()
	ep.Acquire()
	if ep.Load() != 2 {
		t.Errorf("load = %d, want in-flight count 2", ep.Load())
	}
	ep.SetMetrics(&Metrics{Running: 4, Waiting: 1, MaxLoRAs: 1, ActiveAdapters: map[string]struct{}{"a": {}}})
	if ep.Load() != 5 {
		t.Errorf("load = %d, want scraped 5", ep.Load())
	}
	if ep.Adapter("a") != AdapterActive || ep.Adapter("b") != AdapterFull {
		t.Errorf("adapter states: a=%v b=%v", ep.Adapter("a"), ep.Adapter("b"))
	}
	ep.Release()
	ep.Release()
	if ep.InFlight() != 0 {
		t.Error("release did not decrement")
	}

	if _, err := New("10.0.0.1:8000"); err == nil {
		t.Error("a URL without scheme should be rejected")
	}
	if e, _ := New("https://vllm.example"); e.Addr != "vllm.example:443" {
		t.Errorf("default https port: %s", e.Addr)
	}
}

func TestPoolScrapeAndHealth(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(vllmV1)) }))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()

	p, err := NewPool(PoolOptions{Name: "p", URLs: []string{good.URL, bad.URL}})
	if err != nil {
		t.Fatal(err)
	}
	for range unhealthyAfter {
		p.Scrape(context.Background(), http.DefaultClient)
	}
	ready := p.Ready()
	if len(ready) != 1 || ready[0].URL != good.URL || ready[0].Metrics().Running != 7 {
		t.Fatalf("ready = %v", ready)
	}
	good.Close()
	for range unhealthyAfter {
		p.Scrape(context.Background(), http.DefaultClient)
	}
	if len(p.Ready()) != 2 {
		t.Error("with every endpoint unhealthy, Ready should fall back to all of them")
	}
}

func TestPoolDNSRefreshKeepsState(t *testing.T) {
	ips := []string{"10.0.0.2", "10.0.0.1"}
	p, err := NewPool(PoolOptions{Name: "p", DNS: "vllm.ns.svc:8000",
		Lookup: func(_ context.Context, host string) ([]string, error) {
			if host != "vllm.ns.svc" {
				t.Errorf("looked up %q", host)
			}
			return ips, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	p.OnRemove(func(ep *Endpoint) { removed = append(removed, ep.Addr) })
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	eps := p.Endpoints()
	if len(eps) != 2 || eps[0].Addr != "10.0.0.1:8000" || eps[0].URL != "http://10.0.0.1:8000" {
		t.Fatalf("endpoints = %v", eps)
	}
	eps[1].Acquire()

	ips = []string{"10.0.0.2", "10.0.0.3"}
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	eps = p.Endpoints()
	if len(eps) != 2 || eps[0].Addr != "10.0.0.2:8000" || eps[0].InFlight() != 1 || eps[1].Addr != "10.0.0.3:8000" {
		t.Fatalf("after refresh: %v (in-flight %d)", eps, eps[0].InFlight())
	}
	if len(removed) != 1 || removed[0] != "10.0.0.1:8000" {
		t.Errorf("removed = %v", removed)
	}
}
