package mockvllm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
)

func fastConfig() Config {
	cfg := DefaultConfig()
	cfg.Adapters = []string{"a", "b", "c"}
	cfg.MaxLoRAs = 1
	cfg.PrefillTPS = 1e6
	cfg.DecodeTPS = 1e5
	cfg.BaseLatency = 0
	cfg.LoRALoadLatency = 0
	return cfg
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func counter(t *testing.T, url, name string) float64 {
	t.Helper()
	resp, err := http.Get(url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	families, err := endpoint.ParseMetricFamilies(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint.Sum(families, name)
}

func TestPrefixCacheAcrossTurns(t *testing.T) {
	srv := httptest.NewServer(New(fastConfig()).Handler())
	defer srv.Close()
	system := strings.Repeat("You are a helpful assistant. ", 40) // ~1.1 KB, 17 blocks

	turn1 := `{"model":"meta-llama/Llama-3.1-8B-Instruct","max_tokens":40,"messages":[{"role":"system","content":"` + system + `"},{"role":"user","content":"first question"}]}`
	status, body := post(t, srv.URL+"/v1/chat/completions", turn1)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var resp struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if hits := counter(t, srv.URL, "vllm:prefix_cache_hits_total"); hits != 0 {
		t.Errorf("cold cache reported %v hits", hits)
	}
	if n := resp.Usage.CompletionTokens; n < 20 || n > 40 || len(resp.Choices[0].Message.Content) != 4*n {
		t.Errorf("completion: %d tokens, %d chars", n, len(resp.Choices[0].Message.Content))
	}

	// The second turn repeats the history, including the reply: everything up
	// to the new user message should come from the cache.
	reply, _ := json.Marshal(resp.Choices[0].Message.Content)
	turn2 := `{"model":"meta-llama/Llama-3.1-8B-Instruct","max_tokens":8,"messages":[{"role":"system","content":"` + system +
		`"},{"role":"user","content":"first question"},{"role":"assistant","content":` + string(reply) +
		`},{"role":"user","content":"second question"}]}`
	if status, body := post(t, srv.URL+"/v1/chat/completions", turn2); status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	queries := counter(t, srv.URL, "vllm:prefix_cache_queries_total")
	hits := counter(t, srv.URL, "vllm:prefix_cache_hits_total")
	turn2Tokens := queries - float64(resp.Usage.PromptTokens)
	firstLen := float64(resp.Usage.PromptTokens + resp.Usage.CompletionTokens)
	if hits < firstLen-16 || hits > turn2Tokens {
		t.Errorf("turn 2: %v of %v prompt tokens cached, want about the %v tokens of turn 1", hits, turn2Tokens, firstLen)
	}

	// Another adapter does not share the base model's cache.
	before := counter(t, srv.URL, "vllm:prefix_cache_hits_total")
	post(t, srv.URL+"/v1/chat/completions", strings.Replace(turn1, "meta-llama/Llama-3.1-8B-Instruct", "a", 1))
	if after := counter(t, srv.URL, "vllm:prefix_cache_hits_total"); after != before {
		t.Errorf("adapter request hit the base model's cache (%v -> %v)", before, after)
	}
}

func TestStreamingAndUsage(t *testing.T) {
	srv := httptest.NewServer(New(fastConfig()).Handler())
	defer srv.Close()
	_, body := post(t, srv.URL+"/v1/chat/completions",
		`{"model":"b","max_tokens":4,"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	events := strings.Split(strings.TrimSpace(body), "\n\n")
	if events[len(events)-1] != "data: [DONE]" {
		t.Fatalf("stream does not end with [DONE]: %q", body)
	}
	usage := events[len(events)-2]
	if !strings.Contains(usage, `"choices":[]`) || !strings.Contains(usage, `"usage"`) {
		t.Errorf("final chunk before [DONE] should carry usage: %s", usage)
	}
	if !strings.Contains(events[0], `"role":"assistant"`) || !strings.Contains(events[0], `"model":"b"`) {
		t.Errorf("first chunk = %s", events[0])
	}
	_, body = post(t, srv.URL+"/v1/completions", `{"model":"a","max_tokens":2,"stream":true,"prompt":"hello"}`)
	if strings.Contains(body, "usage") || !strings.Contains(body, `"object":"text_completion"`) {
		t.Errorf("completions stream: %s", body)
	}
}

func TestUnknownModel(t *testing.T) {
	srv := httptest.NewServer(New(fastConfig()).Handler())
	defer srv.Close()
	status, body := post(t, srv.URL+"/v1/chat/completions", `{"model":"zzz","messages":[{"role":"user","content":"x"}]}`)
	if status != 404 || !strings.Contains(body, "NotFoundError") {
		t.Errorf("status %d body %s", status, body)
	}
}

func TestLoRAInfoMetric(t *testing.T) {
	s := New(fastConfig())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if _, err := s.lora.acquire(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	m, err := endpoint.ParseVLLM(resp.Body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.ActiveAdapters["a"]; !ok || m.MaxLoRAs != 1 {
		t.Errorf("parsed lora info: %+v", m)
	}
}

func TestLoRAManagerLimitsAndFairness(t *testing.T) {
	loads := 0
	m := newLoRAManager(1, 30*time.Millisecond, func() { loads++ })
	ctx := context.Background()

	if p, err := m.acquire(ctx, "a"); err != nil || p != 30*time.Millisecond {
		t.Fatalf("first load: penalty %v err %v", p, err)
	}
	if p, _ := m.acquire(ctx, "a"); p != 0 {
		t.Errorf("resident adapter should not pay the load penalty, got %v", p)
	}

	// b cannot load while a has in-flight requests.
	gotB := make(chan struct{})
	go func() {
		if _, err := m.acquire(ctx, "b"); err == nil {
			close(gotB)
		}
	}()
	waitFor(t, func() bool { _, w := m.state(); return len(w) == 1 })

	// A later request for the resident adapter queues behind the swap.
	gotA := make(chan struct{})
	go func() {
		if _, err := m.acquire(ctx, "a"); err == nil {
			close(gotA)
		}
	}()
	waitFor(t, func() bool { _, w := m.state(); return len(w) == 2 })
	select {
	case <-gotA:
		t.Fatal("request for a jumped ahead of the blocked swap to b")
	case <-gotB:
		t.Fatal("b loaded while a was busy")
	case <-time.After(20 * time.Millisecond):
	}

	m.release("a")
	m.release("a")
	<-gotB
	if running, _ := m.state(); len(running) != 1 || running[0] != "b" || loads != 2 {
		t.Errorf("after swap: running %v, loads %d", running, loads)
	}
	m.release("b")
	<-gotA
	if loads != 3 {
		t.Errorf("loads = %d, want 3 (a, b, a)", loads)
	}

	// Waiting respects cancellation.
	cctx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { _, err := m.acquire(cctx, "c"); errc <- err }()
	waitFor(t, func() bool { _, w := m.state(); return len(w) == 1 })
	cancel()
	if err := <-errc; err == nil {
		t.Error("cancelled acquire succeeded")
	}
}

func TestPrefillQueueIsFIFO(t *testing.T) {
	q := &prefillQueue{rate: 1000} // 1 token per ms
	start := time.Now()
	first := q.schedule(50)
	second := q.schedule(50)
	if d := second.Sub(first); d < 49*time.Millisecond || d > 51*time.Millisecond {
		t.Errorf("second prefill should start when the first ends: gap %v", d)
	}
	if d := first.Sub(start); d > 55*time.Millisecond {
		t.Errorf("first prefill took %v", d)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
