// Package loadgen drives multi-turn chat traffic from many concurrent
// virtual users through the gateway and measures what users experience
// (time to first token) alongside what the model servers report (prefix
// cache hits).
package loadgen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
)

// Config controls a run.
type Config struct {
	Label  string
	Target string // gateway base URL
	// MetricsURLs are the model servers' /metrics endpoints.
	MetricsURLs []string
	Users       int
	Warmup      time.Duration
	Duration    time.Duration
	TurnsMin    int
	TurnsMax    int
	ThinkMin    time.Duration
	ThinkMax    time.Duration
	Seed        uint64
}

// Result is the summary of one run.
type Result struct {
	Label        string         `json:"label"`
	Users        int            `json:"users"`
	Replicas     int            `json:"replicas"`
	DurationSec  float64        `json:"duration_sec"`
	Requests     int            `json:"requests"`
	Errors       int            `json:"errors"`
	ErrorKinds   map[string]int `json:"error_kinds,omitempty"`
	ReqPerSec    float64        `json:"req_per_sec"`
	OutTokPerSec float64        `json:"output_tokens_per_sec"`
	TTFTp50Ms    float64        `json:"ttft_p50_ms"`
	TTFTp95Ms    float64        `json:"ttft_p95_ms"`
	TTFTp99Ms    float64        `json:"ttft_p99_ms"`
	E2Ep50Ms     float64        `json:"e2e_p50_ms"`
	E2Ep95Ms     float64        `json:"e2e_p95_ms"`
	// Prefix-cache statistics from the model servers' counters.
	PromptTokens   float64 `json:"prompt_tokens"`
	CachedTokens   float64 `json:"cached_tokens"`
	CacheHitRate   float64 `json:"cache_hit_rate"`
	AdapterLoads   float64 `json:"lora_adapter_loads"`
	MaxMeanReplica float64 `json:"replica_load_max_over_mean"`
}

type sample struct {
	ttft, e2e time.Duration
	outTokens int64
	err       string
}

// Run executes the workload and returns its summary.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	tenants := Tenants()
	totalWeight := 0
	for _, t := range tenants {
		totalWeight += t.Weight
	}
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns: cfg.Users * 2, MaxIdleConnsPerHost: cfg.Users * 2, IdleConnTimeout: time.Minute,
		DisableCompression: true,
	}}
	defer client.CloseIdleConnections()

	start := time.Now()
	measureStart := start.Add(cfg.Warmup)
	measureEnd := measureStart.Add(cfg.Duration)
	// In-flight requests may finish after measureEnd; give them a deadline.
	ctx, cancel := context.WithDeadline(ctx, measureEnd.Add(60*time.Second))
	defer cancel()

	var before map[string]counters
	beforeDone := make(chan error, 1)
	go func() {
		time.Sleep(time.Until(measureStart))
		var err error
		before, err = scrapeAll(ctx, cfg.MetricsURLs)
		beforeDone <- err
	}()

	var (
		mu      sync.Mutex
		samples []sample
		wg      sync.WaitGroup
	)
	for i := range cfg.Users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(cfg.Seed, uint64(i)))
			// Stagger arrivals over the first two seconds.
			if !sleepCtx(ctx, time.Duration(r.Int64N(int64(2*time.Second)))) {
				return
			}
			for time.Now().Before(measureEnd) && ctx.Err() == nil {
				t := weighted(r, tenants, totalWeight)
				vars := map[string]any(nil)
				if t.PromptVars != nil {
					vars = t.PromptVars(r)
				}
				var history []map[string]string
				turns := cfg.TurnsMin + r.IntN(cfg.TurnsMax-cfg.TurnsMin+1)
				for turn := 0; turn < turns && time.Now().Before(measureEnd); turn++ {
					history = append(history, map[string]string{"role": "user", "content": t.Ask(r, turn)})
					sent := time.Now()
					s, reply := chat(ctx, client, cfg.Target, t, vars, history)
					if sent.After(measureStart) && ctx.Err() == nil {
						mu.Lock()
						samples = append(samples, s)
						mu.Unlock()
					}
					history = append(history, map[string]string{"role": "assistant", "content": reply})
					think := cfg.ThinkMin + time.Duration(r.Int64N(int64(cfg.ThinkMax-cfg.ThinkMin)+1))
					if !sleepCtx(ctx, think) {
						return
					}
					if s.err != "" {
						break // abandon the conversation
					}
				}
			}
		}()
	}
	wg.Wait()
	if err := <-beforeDone; err != nil {
		return nil, fmt.Errorf("scrape model servers: %w", err)
	}
	after, err := scrapeAll(ctx, cfg.MetricsURLs)
	if err != nil {
		return nil, fmt.Errorf("scrape model servers: %w", err)
	}
	return summarize(cfg, samples, before, after), nil
}

func weighted(r *rand.Rand, tenants []Tenant, total int) *Tenant {
	n := r.IntN(total)
	for i := range tenants {
		if n < tenants[i].Weight {
			return &tenants[i]
		}
		n -= tenants[i].Weight
	}
	return &tenants[len(tenants)-1]
}

// chat sends one streaming turn and returns its measurements and the reply.
func chat(ctx context.Context, client *http.Client, target string, t *Tenant, vars map[string]any,
	history []map[string]string) (sample, string) {
	msgs := make([]map[string]string, 0, len(history)+1)
	if t.System != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": t.System})
	}
	msgs = append(msgs, history...)
	body := map[string]any{
		"model": t.Model, "messages": msgs, "max_tokens": t.MaxTokens,
		"stream": true, "stream_options": map[string]any{"include_usage": true},
	}
	if t.PromptID != "" {
		body["prompt"] = map[string]any{"id": t.PromptID, "label": "production", "variables": vars}
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return sample{err: "request"}, ""
	}
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return sample{err: "transport"}, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return sample{err: fmt.Sprintf("http_%d", resp.StatusCode)}, ""
	}

	var (
		s     sample
		reply strings.Builder
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				if s.ttft == 0 {
					s.ttft = time.Since(start)
				}
				reply.WriteString(c.Delta.Content)
			}
		}
		if chunk.Usage != nil {
			s.outTokens = chunk.Usage.CompletionTokens
		}
	}
	s.e2e = time.Since(start)
	if sc.Err() != nil || s.ttft == 0 {
		s.err = "stream"
	}
	return s, reply.String()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// counters are the model-server metrics the report needs.
type counters struct {
	queries, hits, requests, loads float64
}

func scrapeAll(ctx context.Context, urls []string) (map[string]counters, error) {
	out := map[string]counters{}
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		families, err := endpoint.ParseMetricFamilies(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u, err)
		}
		out[u] = counters{
			queries:  endpoint.Sum(families, "vllm:prefix_cache_queries_total"),
			hits:     endpoint.Sum(families, "vllm:prefix_cache_hits_total"),
			requests: endpoint.Sum(families, "vllm:request_success_total"),
			loads:    endpoint.Sum(families, "mockvllm_lora_adapter_loads_total"),
		}
	}
	return out, nil
}

func summarize(cfg Config, samples []sample, before, after map[string]counters) *Result {
	res := &Result{Label: cfg.Label, Users: cfg.Users, Replicas: len(cfg.MetricsURLs),
		DurationSec: cfg.Duration.Seconds(), ErrorKinds: map[string]int{}}
	var ttfts, e2es []time.Duration
	var outTokens int64
	for _, s := range samples {
		res.Requests++
		if s.err != "" {
			res.Errors++
			res.ErrorKinds[s.err]++
			continue
		}
		ttfts = append(ttfts, s.ttft)
		e2es = append(e2es, s.e2e)
		outTokens += s.outTokens
	}
	res.ReqPerSec = float64(len(ttfts)) / cfg.Duration.Seconds()
	res.OutTokPerSec = float64(outTokens) / cfg.Duration.Seconds()
	res.TTFTp50Ms, res.TTFTp95Ms, res.TTFTp99Ms = ms(percentile(ttfts, 50)), ms(percentile(ttfts, 95)), ms(percentile(ttfts, 99))
	res.E2Ep50Ms, res.E2Ep95Ms = ms(percentile(e2es, 50)), ms(percentile(e2es, 95))

	var maxReq, sumReq float64
	for u, a := range after {
		b := before[u]
		res.PromptTokens += a.queries - b.queries
		res.CachedTokens += a.hits - b.hits
		res.AdapterLoads += a.loads - b.loads
		n := a.requests - b.requests
		sumReq += n
		maxReq = max(maxReq, n)
	}
	if res.PromptTokens > 0 {
		res.CacheHitRate = res.CachedTokens / res.PromptTokens
	}
	if sumReq > 0 {
		res.MaxMeanReplica = maxReq / (sumReq / float64(len(after)))
	}
	return res
}

func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	i := int(float64(len(s))*p/100+0.5) - 1
	return s[min(max(i, 0), len(s)-1)]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
