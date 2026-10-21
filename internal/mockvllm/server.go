// Package mockvllm is a stand-in for a vLLM OpenAI-compatible server, used
// for local development and benchmarking without GPUs. It simulates the
// parts of vLLM that routing decisions affect:
//
//   - automatic prefix caching: prompts are split into KV blocks
//     (BlockTokens tokens each) with chained hashes, cached in an LRU of
//     CacheBlocks blocks; only uncached tokens need prefill
//   - a fixed number of sequence slots (--max-num-seqs) with a FIFO waiting queue
//   - prefill as a shared per-replica resource running at PrefillTPS
//   - decode at DecodeTPS tokens per second per sequence
//   - LoRA adapters, at most MaxLoRAs resident, with a load penalty
//
// so that time to first token = queue wait + prefill(uncached tokens) +
// BaseLatency (+ LoRA load). It exports vLLM-named Prometheus metrics,
// including prefix-cache query/hit counters, so cache hit rates are
// measured at the servers rather than assumed.
//
// Tokenisation is approximated as CharsPerToken characters per token.
package mockvllm

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/openai"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/prefixhash"
)

// Config sets the simulated hardware.
type Config struct {
	Model            string
	Adapters         []string
	MaxLoRAs         int
	MaxNumSeqs       int
	BlockTokens      int
	CharsPerToken    int
	CacheBlocks      int
	PrefillTPS       float64
	DecodeTPS        float64
	BaseLatency      time.Duration
	LoRALoadLatency  time.Duration
	DefaultMaxTokens int
}

// DefaultConfig is loosely an 8B model on a single data-centre GPU.
func DefaultConfig() Config {
	return Config{
		Model:            "meta-llama/Llama-3.1-8B-Instruct",
		MaxLoRAs:         4,
		MaxNumSeqs:       64,
		BlockTokens:      16,
		CharsPerToken:    4,
		CacheBlocks:      8192,
		PrefillTPS:       10000,
		DecodeTPS:        50,
		BaseLatency:      15 * time.Millisecond,
		LoRALoadLatency:  50 * time.Millisecond,
		DefaultMaxTokens: 128,
	}
}

// Server is one simulated replica.
type Server struct {
	cfg     Config
	slots   chan struct{}
	cache   *blockCache
	prefill *prefillQueue
	lora    *loraManager

	waiting    atomic.Int64
	running    atomic.Int64
	heldBlocks atomic.Int64
	seq        atomic.Uint64

	reg          *prometheus.Registry
	queries      prometheus.Counter
	hits         prometheus.Counter
	promptToks   prometheus.Counter
	genToks      prometheus.Counter
	success      prometheus.Counter
	ttft         prometheus.Histogram
	adapterLoads prometheus.Counter
}

// New builds a replica.
func New(cfg Config) *Server {
	s := &Server{
		cfg:     cfg,
		slots:   make(chan struct{}, cfg.MaxNumSeqs),
		cache:   newBlockCache(cfg.CacheBlocks),
		prefill: &prefillQueue{rate: cfg.PrefillTPS},
		reg:     prometheus.NewRegistry(),
	}
	labels := prometheus.Labels{"model_name": cfg.Model}
	counter := func(name, help string) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help, ConstLabels: labels})
		s.reg.MustRegister(c)
		return c
	}
	gauge := func(name, help string, f func() float64) {
		s.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help, ConstLabels: labels}, f))
	}
	s.queries = counter("vllm:prefix_cache_queries_total", "Prompt tokens looked up in the prefix cache.")
	s.hits = counter("vllm:prefix_cache_hits_total", "Prompt tokens found in the prefix cache.")
	s.promptToks = counter("vllm:prompt_tokens_total", "Prefill tokens processed.")
	s.genToks = counter("vllm:generation_tokens_total", "Generation tokens processed.")
	s.success = counter("vllm:request_success_total", "Successfully finished requests.")
	s.adapterLoads = counter("mockvllm_lora_adapter_loads_total", "LoRA adapter loads (simulation-only metric).")
	gauge("vllm:num_requests_running", "Requests in model execution batches.", func() float64 { return float64(s.running.Load()) })
	gauge("vllm:num_requests_waiting", "Requests waiting to be processed.", func() float64 { return float64(s.waiting.Load()) })
	gauge("vllm:kv_cache_usage_perc", "KV-cache usage of running requests (1 = 100%).", func() float64 {
		return min(1, float64(s.heldBlocks.Load())/float64(cfg.CacheBlocks))
	})
	s.ttft = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "vllm:time_to_first_token_seconds", Help: "Time to first token.", ConstLabels: labels,
		Buckets: []float64{.01, .025, .05, .1, .2, .3, .5, .75, 1, 1.5, 2, 3, 5, 10},
	})
	s.reg.MustRegister(s.ttft, loraInfo{s})
	s.lora = newLoRAManager(cfg.MaxLoRAs, cfg.LoRALoadLatency, s.adapterLoads.Inc)
	return s
}

// Handler serves the OpenAI-compatible API, /metrics and /health.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { s.complete(w, r, true) })
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) { s.complete(w, r, false) })
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{}))
	return mux
}

type request struct {
	Model               string           `json:"model"`
	Messages            []openai.Message `json:"messages"`
	Prompt              json.RawMessage  `json:"prompt"`
	MaxTokens           *int             `json:"max_tokens"`
	MaxCompletionTokens *int             `json:"max_completion_tokens"`
	Stream              bool             `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request, chat bool) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		vllmError(w, http.StatusBadRequest, "BadRequestError", err.Error())
		return
	}
	adapter := ""
	switch {
	case req.Model == s.cfg.Model:
	case slices.Contains(s.cfg.Adapters, req.Model):
		adapter = req.Model
	default:
		vllmError(w, http.StatusNotFound, "NotFoundError", fmt.Sprintf("The model `%s` does not exist.", req.Model))
		return
	}
	var prompt string
	if chat {
		if len(req.Messages) == 0 {
			vllmError(w, http.StatusBadRequest, "BadRequestError", "messages is required")
			return
		}
		prompt = chatTemplate(req.Messages)
	} else if err := json.Unmarshal(req.Prompt, &prompt); err != nil {
		vllmError(w, http.StatusBadRequest, "BadRequestError", "prompt must be a string")
		return
	}
	maxTokens := s.cfg.DefaultMaxTokens
	if req.MaxCompletionTokens != nil {
		maxTokens = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	s.generate(r.Context(), w, &req, chat, adapter, prompt, max(maxTokens, 1))
}

// chatTemplate renders messages in Llama 3 chat format. A conversation's
// next prompt (history + reply + new turn) extends the previous prompt plus
// its completion, so multi-turn requests can reuse cached blocks.
func chatTemplate(msgs []openai.Message) string {
	var b strings.Builder
	b.WriteString("<|begin_of_text|>")
	for _, m := range msgs {
		b.WriteString("<|start_header_id|>" + m.Role + "<|end_header_id|>\n\n")
		b.WriteString(m.Text())
		b.WriteString("<|eot_id|>")
	}
	b.WriteString("<|start_header_id|>assistant<|end_header_id|>\n\n")
	return b.String()
}

// admit waits for the adapter (if any) and a sequence slot.
func (s *Server) admit(ctx context.Context, adapter string) (release func(), penalty time.Duration, err error) {
	s.waiting.Add(1)
	defer s.waiting.Add(-1)
	if adapter != "" {
		if penalty, err = s.lora.acquire(ctx, adapter); err != nil {
			return nil, 0, err
		}
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		if adapter != "" {
			s.lora.release(adapter)
		}
		return nil, 0, ctx.Err()
	}
	s.running.Add(1)
	return func() {
		s.running.Add(-1)
		<-s.slots
		if adapter != "" {
			s.lora.release(adapter)
		}
	}, penalty, nil
}

func (s *Server) generate(ctx context.Context, w http.ResponseWriter, req *request, chat bool, adapter, prompt string, maxTokens int) {
	arrival := time.Now()
	release, penalty, err := s.admit(ctx, adapter)
	if err != nil {
		return // client went away while queued
	}
	defer release()

	cfg := s.cfg
	blockChars := cfg.BlockTokens * cfg.CharsPerToken
	promptTokens := (len(prompt) + cfg.CharsPerToken - 1) / cfg.CharsPerToken
	blocks := prefixhash.Blocks(req.Model, prompt, blockChars, 0)
	// Like vLLM, the last prompt token is always recomputed.
	cached := min(s.cache.match(blocks)*cfg.BlockTokens, max(promptTokens-1, 0))
	s.queries.Add(float64(promptTokens))
	s.hits.Add(float64(cached))
	s.promptToks.Add(float64(promptTokens))

	held := int64(len(blocks) + 1)
	s.heldBlocks.Add(held)
	defer func() { s.heldBlocks.Add(-held) }()

	firstToken := s.prefill.schedule(promptTokens - cached).Add(cfg.BaseLatency + penalty)
	if !sleepUntil(ctx, firstToken) {
		return
	}
	s.cache.insert(blocks)
	s.ttft.Observe(time.Since(arrival).Seconds())

	tokens := s.completion(prompt, maxTokens)
	finish := "stop"
	if len(tokens) == maxTokens {
		finish = "length"
	}
	usage := openai.Usage{PromptTokens: int64(promptTokens), CompletionTokens: int64(len(tokens))}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	interval := time.Duration(float64(time.Second) / cfg.DecodeTPS)
	idPrefix := "cmpl"
	if chat {
		idPrefix = "chatcmpl"
	}
	id := fmt.Sprintf("%s-%d", idPrefix, s.seq.Add(1))
	created := arrival.Unix()

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		send := func(v any) bool {
			b, _ := json.Marshal(v)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return false
			}
			if flusher != nil {
				flusher.Flush()
			}
			return true
		}
		for i, tok := range tokens {
			if i > 0 && !sleepUntil(ctx, firstToken.Add(time.Duration(i)*interval)) {
				return
			}
			if (i+1)%cfg.BlockTokens == 0 {
				s.heldBlocks.Add(1)
				held++
			}
			if !send(chunk(id, req.Model, created, chat, i == 0, tok, nil)) {
				return
			}
		}
		send(chunk(id, req.Model, created, chat, false, "", &finish))
		if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
			send(map[string]any{"id": id, "object": objectName(chat, true), "created": created, "model": req.Model,
				"choices": []any{}, "usage": usage})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	} else {
		if !sleepUntil(ctx, firstToken.Add(time.Duration(len(tokens)-1)*interval)) {
			return
		}
		text := strings.Join(tokens, "")
		choice := map[string]any{"index": 0, "finish_reason": finish, "logprobs": nil}
		if chat {
			choice["message"] = map[string]any{"role": "assistant", "content": text}
		} else {
			choice["text"] = text
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": objectName(chat, false), "created": created,
			"model": req.Model, "choices": []any{choice}, "usage": usage})
	}
	// The finished sequence's blocks (prompt + completion) stay cached, so the
	// next turn of the conversation can hit them.
	s.cache.insert(prefixhash.Blocks(req.Model, prompt+strings.Join(tokens, ""), blockChars, 0))
	s.genToks.Add(float64(len(tokens)))
	s.success.Inc()
}

func chunk(id, model string, created int64, chat, first bool, text string, finish *string) map[string]any {
	choice := map[string]any{"index": 0, "logprobs": nil, "finish_reason": finish}
	if chat {
		delta := map[string]any{}
		if first {
			delta["role"] = "assistant"
		}
		if text != "" {
			delta["content"] = text
		}
		choice["delta"] = delta
	} else {
		choice["text"] = text
	}
	return map[string]any{"id": id, "object": objectName(chat, true), "created": created, "model": model, "choices": []any{choice}}
}

func objectName(chat, stream bool) string {
	switch {
	case chat && stream:
		return "chat.completion.chunk"
	case chat:
		return "chat.completion"
	}
	return "text_completion"
}

// vocabulary of pseudo tokens; each is padded to CharsPerToken characters.
var vocabulary = strings.Fields("the and for you can use new get set run pod api key log fix add now one two all any may our its")

// completion deterministically generates between half and all of maxTokens
// tokens, seeded by the prompt, each exactly CharsPerToken characters.
func (s *Server) completion(prompt string, maxTokens int) []string {
	h := fnv.New64a()
	h.Write([]byte(prompt))
	rng := rand.New(rand.NewPCG(h.Sum64(), 0x9e3779b97f4a7c15))
	n := maxTokens/2 + rng.IntN(maxTokens-maxTokens/2) + 1
	n = min(n, maxTokens)
	out := make([]string, n)
	for i := range out {
		w := vocabulary[rng.IntN(len(vocabulary))] + " "
		for len(w) < s.cfg.CharsPerToken {
			w += " "
		}
		out[i] = w[:s.cfg.CharsPerToken]
	}
	return out
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	data := []map[string]any{{"id": s.cfg.Model, "object": "model", "owned_by": "vllm", "root": s.cfg.Model}}
	for _, a := range s.cfg.Adapters {
		data = append(data, map[string]any{"id": a, "object": "model", "owned_by": "vllm", "root": a, "parent": s.cfg.Model})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// vllmError writes vLLM's error body format.
func vllmError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "error", "message": msg, "type": typ, "param": nil, "code": status})
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// loraInfo exports vllm:lora_requests_info, whose labels list the adapters
// in use and whose value is the time of the update, as vLLM does.
type loraInfo struct{ s *Server }

var loraInfoDesc = prometheus.NewDesc("vllm:lora_requests_info", "Running stats on LoRA requests.",
	[]string{"max_lora", "running_lora_adapters", "waiting_lora_adapters"}, nil)

func (l loraInfo) Describe(ch chan<- *prometheus.Desc) { ch <- loraInfoDesc }

func (l loraInfo) Collect(ch chan<- prometheus.Metric) {
	running, waiting := l.s.lora.state()
	ch <- prometheus.MustNewConstMetric(loraInfoDesc, prometheus.GaugeValue, float64(time.Now().Unix()),
		fmt.Sprint(l.s.cfg.MaxLoRAs), strings.Join(running, ","), strings.Join(waiting, ","))
}
