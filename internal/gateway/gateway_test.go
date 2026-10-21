package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/auth"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/budget"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/config"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/picker"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/prompts"
)

const testPrompt = `
id: helper
labels: {production: 2}
versions:
  - version: 1
    messages: [{role: system, content: "old"}]
  - version: 2
    params: {temperature: 0.1, top_p: 0.9}
    messages: [{role: system, content: "You help {{ .team }}."}]
  - version: 3
    text: "Summarise for {{ .team }}:"
`

// upstream is a fake model server that records request bodies.
type upstream struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	reply  func(w http.ResponseWriter, body map[string]any)
}

func (u *upstream) last(t *testing.T) map[string]any {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		t.Fatal("upstream was not called")
	}
	return u.bodies[len(u.bodies)-1]
}

func (u *upstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

func jsonReply(usage int) func(w http.ResponseWriter, body map[string]any) {
	return func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"x","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":%d,"completion_tokens":2,"total_tokens":%d}}`,
			body["model"], usage-2, usage)
	}
}

type harness struct {
	gw       *httptest.Server
	up       *upstream
	mr       *miniredis.Miniredis
	failOpen bool
}

func newHarness(t *testing.T, failOpen bool) *harness {
	t.Helper()
	up := &upstream{reply: jsonReply(30)}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		up.mu.Lock()
		up.bodies = append(up.bodies, body)
		up.mu.Unlock()
		up.reply(w, body)
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "helper.yaml"), []byte(testPrompt), 0o644); err != nil {
		t.Fatal(err)
	}
	registry, err := prompts.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	mode := "closed"
	if failOpen {
		mode = "open"
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
budgets: {failure_mode: %s, default_max_tokens: 64}
picker: {strategy: round-robin}
pools:
  - name: llama
    base_model: meta-llama/Llama-3.1-8B-Instruct
    endpoints: [%s]
    adapters: [support-lora-v2]
models:
  - {name: llama-3.1-8b-instruct, pool: llama}
tenants:
  - id: acme
    api_keys: [%s]
    models: [llama-3.1-8b-instruct]
    aliases: {support-bot: {model: llama-3.1-8b-instruct, adapter: support-lora-v2}}
    tokens_per_minute: 100000
  - id: tiny
    api_keys: [%s]
    models: ["*"]
    tokens_per_minute: 100
`, mode, up.URL, auth.HashKey("sk-acme"), auth.HashKey("sk-tiny"))))
	if err != nil {
		t.Fatal(err)
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { rdb.Close() })
	pool, err := endpoint.NewPool(endpoint.PoolOptions{Name: "llama", URLs: cfg.Pools[0].Endpoints})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{
		Config:    cfg,
		Upstreams: map[string]Upstream{"llama": {Pool: pool, Picker: &picker.RoundRobin{}}},
		Limiter:   budget.NewLimiter(rdb, "t", failOpen, 200*time.Millisecond),
		Prompts:   registry,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(gw.Close)
	return &harness{gw: gw, up: up, mr: mr, failOpen: failOpen}
}

func (h *harness) post(t *testing.T, path, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.gw.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// minuteUsage is the tenant's current-minute budget bucket.
func (h *harness) minuteUsage(t *testing.T, tenant string) int {
	t.Helper()
	for _, k := range h.mr.Keys() {
		if strings.HasPrefix(k, "t:{"+tenant+"}:tpm:") {
			v, _ := h.mr.Get(k)
			n, _ := strconv.Atoi(v)
			return n
		}
	}
	return 0
}

func decodeError(t *testing.T, resp *http.Response) (typ, code string) {
	t.Helper()
	var e struct {
		Error struct {
			Type string  `json:"type"`
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("error body: %v", err)
	}
	if e.Error.Code == nil {
		return e.Error.Type, ""
	}
	return e.Error.Type, *e.Error.Code
}

func TestAuthentication(t *testing.T) {
	h := newHarness(t, true)
	for _, key := range []string{"", "sk-wrong"} {
		resp := h.post(t, "/v1/chat/completions", key, `{}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("key %q: status %d", key, resp.StatusCode)
		}
		if typ, code := decodeError(t, resp); typ != "authentication_error" || code != "invalid_api_key" {
			t.Errorf("error = %s/%s", typ, code)
		}
	}
}

func TestBadRequests(t *testing.T) {
	h := newHarness(t, true)
	cases := map[string]struct {
		body   string
		status int
		code   string
	}{
		"not json":          {`{`, 400, ""},
		"no model":          {`{"messages":[{"role":"user","content":"x"}]}`, 400, ""},
		"unknown model":     {`{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}`, 404, "model_not_found"},
		"not allowed":       {`{"model":"meta-llama/Llama-3.1-8B-Instruct","messages":[{"role":"user","content":"x"}]}`, 404, "model_not_found"},
		"no messages":       {`{"model":"llama-3.1-8b-instruct"}`, 400, ""},
		"bad max_tokens":    {`{"model":"llama-3.1-8b-instruct","max_tokens":-1,"messages":[{"role":"user","content":"x"}]}`, 400, ""},
		"string prompt ref": {`{"model":"llama-3.1-8b-instruct","prompt":"helper","messages":[{"role":"user","content":"x"}]}`, 400, ""},
		"unknown prompt":    {`{"model":"llama-3.1-8b-instruct","prompt":{"id":"nope"}}`, 404, "prompt_not_found"},
		"missing variable":  {`{"model":"llama-3.1-8b-instruct","prompt":{"id":"helper"}}`, 400, ""},
	}
	for name, tc := range cases {
		resp := h.post(t, "/v1/chat/completions", "sk-acme", tc.body)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, tc.status)
			continue
		}
		if _, code := decodeError(t, resp); code != tc.code {
			t.Errorf("%s: code %q, want %q", name, code, tc.code)
		}
	}
	if h.up.calls() != 0 {
		t.Errorf("invalid requests reached the upstream %d times", h.up.calls())
	}
}

func TestLoRARewriteAndUnknownFieldsPassThrough(t *testing.T) {
	h := newHarness(t, true)
	resp := h.post(t, "/v1/chat/completions", "sk-acme",
		`{"model":"support-bot","messages":[{"role":"user","content":"hello"}],"user":"u-1","vendor_ext":{"keep":[1,2]},"temperature":0.7}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got := h.up.last(t)
	if got["model"] != "support-lora-v2" {
		t.Errorf("upstream model = %v, want the adapter name", got["model"])
	}
	if got["user"] != "u-1" || fmt.Sprint(got["vendor_ext"]) != "map[keep:[1 2]]" || got["temperature"] != 0.7 {
		t.Errorf("fields not passed through: %v", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"model":"support-lora-v2"`) || !strings.Contains(string(body), `"total_tokens":30`) {
		t.Errorf("response not relayed verbatim: %s", body)
	}
	if resp.Header.Get("X-Request-Id") == "" || resp.Header.Get("X-Ratelimit-Limit-Tokens") != "100000" {
		t.Errorf("headers: %v", resp.Header)
	}
	if got := h.minuteUsage(t, "acme"); got != 30 {
		t.Errorf("budget after reconcile = %d, want actual usage 30", got)
	}
}

func TestVersionedPromptRendering(t *testing.T) {
	h := newHarness(t, true)
	resp := h.post(t, "/v1/chat/completions", "sk-acme", `{"model":"llama-3.1-8b-instruct","top_p":0.5,
		"prompt":{"id":"helper","label":"production","variables":{"team":"billing"}},
		"messages":[{"role":"user","content":"why is my bill high?"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Prompt-Id") != "helper" || resp.Header.Get("X-Prompt-Version") != "2" {
		t.Errorf("prompt headers: id=%q version=%q", resp.Header.Get("X-Prompt-Id"), resp.Header.Get("X-Prompt-Version"))
	}
	got := h.up.last(t)
	if _, ok := got["prompt"]; ok {
		t.Error("the prompt extension field must not reach the model server")
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 || fmt.Sprint(msgs[0]) != "map[content:You help billing. role:system]" ||
		fmt.Sprint(msgs[1]) != "map[content:why is my bill high? role:user]" {
		t.Errorf("messages = %v", msgs)
	}
	if got["temperature"] != 0.1 || got["top_p"] != 0.5 {
		t.Errorf("prompt params should fill gaps only: temperature=%v top_p=%v", got["temperature"], got["top_p"])
	}

	// An explicit version works for /v1/completions too.
	resp = h.post(t, "/v1/completions", "sk-acme",
		`{"model":"llama-3.1-8b-instruct","prompt":{"id":"helper","version":3,"variables":{"team":"ops"}}}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Prompt-Version") != "3" {
		t.Fatalf("completions: status %d version %q", resp.StatusCode, resp.Header.Get("X-Prompt-Version"))
	}
	if got := h.up.last(t); got["prompt"] != "Summarise for ops:" {
		t.Errorf("completions prompt = %v", got["prompt"])
	}
}

// sse writes server-sent events, flushing each one.
func sse(w http.ResponseWriter, events ...string) {
	for _, e := range events {
		fmt.Fprintf(w, "data: %s\n\n", e)
		w.(http.Flusher).Flush()
	}
}

func TestStreamingFlushesPerChunkAndStripsUsage(t *testing.T) {
	h := newHarness(t, true)
	release := make(chan struct{})
	h.up.reply = func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`)
		<-release // the client must see the first chunk before the stream ends
		sse(w, `{"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":2,"total_tokens":42}}`,
			`[DONE]`)
	}
	resp := h.post(t, "/v1/chat/completions", "sk-acme",
		`{"model":"llama-3.1-8b-instruct","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	rd := bufio.NewReader(resp.Body)
	first := make(chan string, 1)
	go func() {
		line, _ := rd.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if !strings.Contains(line, `"Hel"`) {
			t.Fatalf("first line = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first chunk was not flushed to the client")
	}
	close(release)
	rest, _ := io.ReadAll(rd)
	if strings.Contains(string(rest), "usage") {
		t.Errorf("usage chunk forwarded although the client did not ask for it:\n%s", rest)
	}
	if !strings.Contains(string(rest), `"lo"`) || !strings.HasSuffix(string(rest), "data: [DONE]\n\n") {
		t.Errorf("rest of stream:\n%s", rest)
	}
	opts := h.up.last(t)["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Errorf("gateway should request usage from upstream, sent %v", opts)
	}
	if got := h.minuteUsage(t, "acme"); got != 42 {
		t.Errorf("budget after stream = %d, want 42 from the usage chunk", got)
	}
}

func TestStreamingKeepsRequestedUsage(t *testing.T) {
	h := newHarness(t, true)
	h.up.reply = func(w http.ResponseWriter, _ map[string]any) {
		sse(w, `{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`, `[DONE]`)
	}
	resp := h.post(t, "/v1/chat/completions", "sk-acme",
		`{"model":"llama-3.1-8b-instruct","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"total_tokens":6`) {
		t.Errorf("requested usage chunk missing:\n%s", body)
	}
}

func TestStreamingWithoutUsageCountsChunks(t *testing.T) {
	h := newHarness(t, true)
	h.up.reply = func(w http.ResponseWriter, _ map[string]any) {
		sse(w, `{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"b"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"c"}}]}`)
		// connection ends without a usage chunk or [DONE]
	}
	resp := h.post(t, "/v1/chat/completions", "sk-acme",
		`{"model":"llama-3.1-8b-instruct","stream":true,"messages":[{"role":"user","content":"abcdefgh"}]}`)
	_, _ = io.ReadAll(resp.Body)
	// Estimate: 3 framing + (4 + 8/4) for the message = 9 prompt tokens, plus 3 chunks.
	if got := h.minuteUsage(t, "acme"); got != 12 {
		t.Errorf("budget = %d, want 9 estimated prompt tokens + 3 chunks", got)
	}
}

func TestBudgetRejection(t *testing.T) {
	h := newHarness(t, true)
	req := `{"model":"llama-3.1-8b-instruct","max_tokens":60,"messages":[{"role":"user","content":"hi"}]}`
	h.up.reply = jsonReply(80) // actual usage close to the reservation
	if resp := h.post(t, "/v1/chat/completions", "sk-tiny", req); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: status %d", resp.StatusCode)
	}
	resp := h.post(t, "/v1/chat/completions", "sk-tiny", req)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: status %d, want 429", resp.StatusCode)
	}
	if typ, code := decodeError(t, resp); typ != "rate_limit_error" || code != "rate_limit_exceeded" {
		t.Errorf("error = %s/%s", typ, code)
	}
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || ra < 1 || ra > 120 {
		t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	if resp.Header.Get("X-Ratelimit-Limit-Tokens") != "100" || resp.Header.Get("X-Ratelimit-Remaining-Tokens") != "20" ||
		resp.Header.Get("X-Ratelimit-Reset-Tokens") == "" {
		t.Errorf("rate-limit headers: %v", resp.Header)
	}
	if h.up.calls() != 1 {
		t.Errorf("rejected request reached the upstream (%d calls)", h.up.calls())
	}

	resp = h.post(t, "/v1/chat/completions", "sk-tiny",
		`{"model":"llama-3.1-8b-instruct","max_tokens":500,"messages":[{"role":"user","content":"hi"}]}`)
	if _, code := decodeError(t, resp); resp.StatusCode != 429 || code != "request_too_large" || resp.Header.Get("Retry-After") != "" {
		t.Errorf("oversized request: status %d code %s", resp.StatusCode, code)
	}
}

func TestRedisOutage(t *testing.T) {
	for _, failOpen := range []bool{true, false} {
		h := newHarness(t, failOpen)
		h.mr.Close()
		resp := h.post(t, "/v1/chat/completions", "sk-acme",
			`{"model":"llama-3.1-8b-instruct","messages":[{"role":"user","content":"hi"}]}`)
		want := http.StatusServiceUnavailable
		if failOpen {
			want = http.StatusOK
		}
		if resp.StatusCode != want {
			t.Errorf("failOpen=%v: status %d, want %d", failOpen, resp.StatusCode, want)
		}
	}
}

func TestUpstreamErrorRefundsAndPassesThrough(t *testing.T) {
	h := newHarness(t, true)
	h.up.reply = func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"object":"error","message":"max_tokens too large","type":"BadRequestError","code":400}`)
	}
	resp := h.post(t, "/v1/chat/completions", "sk-acme",
		`{"model":"llama-3.1-8b-instruct","messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "max_tokens too large") {
		t.Errorf("status %d body %s", resp.StatusCode, body)
	}
	if got := h.minuteUsage(t, "acme"); got != 0 {
		t.Errorf("failed request should be refunded, bucket = %d", got)
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	h := newHarness(t, true)
	h.up.Close()
	resp := h.post(t, "/v1/chat/completions", "sk-acme",
		`{"model":"llama-3.1-8b-instruct","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
}

func TestListModels(t *testing.T) {
	h := newHarness(t, true)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, h.gw.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-acme")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Data) != 2 || list.Data[0].ID != "llama-3.1-8b-instruct" || list.Data[1].ID != "support-bot" {
		t.Errorf("models = %+v", list.Data)
	}
}
