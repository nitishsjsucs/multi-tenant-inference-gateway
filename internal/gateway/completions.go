package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/budget"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/config"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/openai"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/picker"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/prompts"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/routing"
)

type kind int

const (
	kindChat kind = iota // /v1/chat/completions
	kindText             // /v1/completions
)

func (k kind) path() string {
	if k == kindChat {
		return "/v1/chat/completions"
	}
	return "/v1/completions"
}

const maxRequestBody = 16 << 20

// apiError is an error destined for the client.
type apiError struct {
	status          int
	typ, code, prop string
	msg             string
}

func (e *apiError) write(w http.ResponseWriter) {
	openai.WriteError(w, e.status, e.typ, e.code, e.prop, e.msg)
}

func badRequest(param, format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, typ: openai.TypeInvalidRequest, prop: param, msg: fmt.Sprintf(format, args...)}
}

// prepared is a request that has been rendered, routed and rewritten.
type prepared struct {
	kind         kind
	body         openai.Body
	route        routing.Route
	prompt       string // canonical prompt text, for prefix routing
	promptTokens int64  // estimate
	cost         int64  // tokens to reserve
	stream       bool
	wantsUsage   bool // client asked for stream_options.include_usage
	rendered     *prompts.Rendered
}

func (s *Server) completions(k kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = newRequestID()
		}
		w.Header().Set("X-Request-Id", reqID)

		tenant, err := s.auth.Authenticate(r)
		if err != nil {
			openai.WriteError(w, http.StatusUnauthorized, openai.TypeAuthentication, "invalid_api_key", "", err.Error())
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			badRequest("", "could not read request body: %v", err).write(w)
			return
		}
		p, apiErr := s.prepare(k, tenant, raw)
		if apiErr != nil {
			s.metrics.requests.WithLabelValues(tenant.ID, "", strconv.Itoa(apiErr.status)).Inc()
			apiErr.write(w)
			return
		}
		model := p.route.Requested
		if p.rendered != nil {
			w.Header().Set("X-Prompt-Id", p.rendered.ID)
			w.Header().Set("X-Prompt-Version", strconv.Itoa(p.rendered.Version))
		}

		status, actual := s.serve(w, r, tenant, p, reqID, start)
		s.metrics.requests.WithLabelValues(tenant.ID, model, strconv.Itoa(status)).Inc()
		s.metrics.duration.WithLabelValues(model, strconv.FormatBool(p.stream)).Observe(time.Since(start).Seconds())
		if actual != nil {
			s.metrics.tokens.WithLabelValues(tenant.ID, model, "prompt").Add(float64(actual.PromptTokens))
			s.metrics.tokens.WithLabelValues(tenant.ID, model, "completion").Add(float64(actual.CompletionTokens))
		}
		s.log.Debug("request", "id", reqID, "tenant", tenant.ID, "model", model, "upstream", p.route.Upstream,
			"status", status, "duration", time.Since(start))
	}
}

// serve reserves budget, picks an endpoint, proxies the request and
// reconciles the budget. It returns the status sent and the token usage.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, tenant *config.Tenant, p *prepared,
	reqID string, start time.Time) (int, *openai.Usage) {
	limits := budget.Limits{TokensPerMinute: tenant.TokensPerMinute, TokensPerDay: tenant.TokensPerDay}
	dec, err := s.limiter.Reserve(r.Context(), tenant.ID, limits, p.cost)
	if err != nil {
		openai.WriteError(w, http.StatusServiceUnavailable, openai.TypeUnavailable, "budget_unavailable", "",
			"token budget service is unavailable; retry later")
		return http.StatusServiceUnavailable, nil
	}
	setRateLimitHeaders(w.Header(), dec)
	if !dec.Allowed {
		s.metrics.budgetRejections.WithLabelValues(tenant.ID, string(dec.Reason)).Inc()
		writeBudgetRejection(w, dec, p.cost)
		return http.StatusTooManyRequests, nil
	}
	// Reconcile even if the client goes away mid-stream.
	settle := func(actual int64) {
		if err := s.limiter.Reconcile(context.WithoutCancel(r.Context()), dec.Reservation, actual); err != nil {
			s.log.Warn("budget reconcile failed", "tenant", tenant.ID, "err", err)
		}
	}

	up, ok := s.upstreams[p.route.Pool]
	if !ok {
		settle(0)
		openai.WriteError(w, http.StatusServiceUnavailable, openai.TypeUnavailable, "no_pool", "", "no upstream pool for model")
		return http.StatusServiceUnavailable, nil
	}
	choice, err := up.Picker.Pick(&picker.Request{Model: p.route.Upstream, Adapter: p.route.Adapter, Prompt: p.prompt}, up.Pool.Ready())
	if err != nil {
		settle(0)
		openai.WriteError(w, http.StatusServiceUnavailable, openai.TypeUnavailable, "no_endpoints", "", err.Error())
		return http.StatusServiceUnavailable, nil
	}
	ep := choice.Endpoint
	s.metrics.pickerDecisions.WithLabelValues(p.route.Pool, up.Picker.Name(), ep.Addr).Inc()
	if choice.TotalBlocks > 0 {
		s.metrics.prefixMatch.WithLabelValues(p.route.Pool).Observe(float64(choice.MatchedBlocks) / float64(choice.TotalBlocks))
	}
	ep.Acquire()
	defer ep.Release()
	s.metrics.inflight.Inc()
	defer s.metrics.inflight.Dec()

	payload, err := p.body.Marshal()
	if err != nil {
		settle(0)
		openai.WriteError(w, http.StatusInternalServerError, openai.TypeServer, "", "", "could not encode request")
		return http.StatusInternalServerError, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UpstreamTimeout)
	defer cancel()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL+p.kind.path(), bytes.NewReader(payload))
	if err != nil {
		settle(0)
		openai.WriteError(w, http.StatusInternalServerError, openai.TypeServer, "", "", err.Error())
		return http.StatusInternalServerError, nil
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("X-Request-Id", reqID)
	// Lets a downstream Gateway API Inference Extension picker apply
	// per-tenant fairness when the gateway is chained in front of Envoy.
	upReq.Header.Set("X-Gateway-Inference-Fairness-Id", tenant.ID)

	resp, err := s.client.Do(upReq)
	if err != nil {
		settle(0)
		s.metrics.upstreamErrors.WithLabelValues(p.route.Pool, ep.Addr).Inc()
		if r.Context().Err() != nil {
			return 499, nil // client closed the request
		}
		openai.WriteError(w, http.StatusBadGateway, openai.TypeServer, "upstream_error", "", "model server request failed")
		return http.StatusBadGateway, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Upstream rejected the request (bad parameters, overload, ...):
		// nothing was generated, so refund the reservation and relay as-is.
		settle(0)
		relayError(w, resp)
		return resp.StatusCode, nil
	}

	var usage *openai.Usage
	status := http.StatusOK
	if p.stream {
		res := relayStream(w, resp.Body, !p.wantsUsage, func() {
			s.metrics.ttft.WithLabelValues(p.route.Requested, up.Picker.Name()).Observe(time.Since(start).Seconds())
		})
		usage = res.usage
		if usage == nil {
			// No usage chunk (e.g. the stream was cut): charge what we saw,
			// counting each content chunk as one token as vLLM emits them.
			usage = &openai.Usage{PromptTokens: p.promptTokens, CompletionTokens: res.chunks}
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
	} else {
		usage, status = relayBuffered(w, resp)
	}
	if usage != nil {
		settle(usage.TotalTokens)
	} else {
		settle(p.promptTokens)
	}
	return status, usage
}

// prepare validates the body, renders any prompt reference, resolves the
// model and rewrites the body for the model server.
func (s *Server) prepare(k kind, tenant *config.Tenant, raw []byte) (*prepared, *apiError) {
	body, err := openai.ParseBody(raw)
	if err != nil {
		return nil, badRequest("", "%v", err)
	}
	p := &prepared{kind: k, body: body}

	var model string
	if ok, err := body.Get("model", &model); err != nil || !ok || model == "" {
		return nil, badRequest("model", "you must provide a model parameter")
	}
	route, err := s.resolver.Resolve(tenant, model)
	if err != nil {
		return nil, &apiError{status: http.StatusNotFound, typ: openai.TypeInvalidRequest, code: "model_not_found", prop: "model",
			msg: fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", model)}
	}
	p.route = route
	if err := body.Set("model", route.Upstream); err != nil {
		return nil, badRequest("model", "%v", err)
	}

	if apiErr := s.applyPrompt(k, p); apiErr != nil {
		return nil, apiErr
	}

	switch k {
	case kindChat:
		var msgs []openai.Message
		if ok, err := body.Get("messages", &msgs); err != nil || !ok || len(msgs) == 0 {
			return nil, badRequest("messages", "messages must be a non-empty array (or reference a prompt)")
		}
		p.prompt = openai.ChatPrompt(msgs)
		p.promptTokens = openai.EstimateChatTokens(msgs)
	case kindText:
		text, ok := completionPrompt(body)
		if !ok {
			return nil, badRequest("prompt", "prompt must be a string, an array of strings, or a prompt reference")
		}
		p.prompt = text
		p.promptTokens = openai.EstimateTokens(text)
	}

	if _, err := body.Get("stream", &p.stream); err != nil {
		return nil, badRequest("stream", "%v", err)
	}
	if p.stream {
		opts := map[string]any{}
		if _, err := body.Get("stream_options", &opts); err != nil {
			return nil, badRequest("stream_options", "%v", err)
		}
		p.wantsUsage, _ = opts["include_usage"].(bool)
		// Always ask upstream for a final usage chunk so the budget can be
		// reconciled exactly; it is stripped again if the client didn't ask.
		opts["include_usage"] = true
		_ = body.Set("stream_options", opts)
	}

	maxTokens := int64(s.cfg.Budgets.DefaultMaxTokens)
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		var v int64
		if ok, err := body.Get(key, &v); err != nil || v < 0 {
			return nil, badRequest(key, "%s must be a non-negative integer", key)
		} else if ok {
			maxTokens = v
			break
		}
	}
	n := int64(1)
	if ok, err := body.Get("n", &n); err != nil || (ok && n < 1) {
		return nil, badRequest("n", "n must be a positive integer")
	}
	if maxTokens > math.MaxInt64/(n+1) {
		return nil, badRequest("max_tokens", "max_tokens is too large")
	}
	p.cost = p.promptTokens + maxTokens*n
	return p, nil
}

// applyPrompt renders a {"prompt": {"id": ...}} reference. For chat the
// rendered messages go in front of the request's own messages; for
// completions the rendered text becomes the prompt.
func (s *Server) applyPrompt(k kind, p *prepared) *apiError {
	raw, ok := p.body["prompt"]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		if ok && k == kindChat {
			return badRequest("prompt", "prompt must be an object such as {\"id\": \"...\", \"label\": \"production\"}")
		}
		return nil // completions prompt string/array, or no reference
	}
	var ref prompts.Ref
	if err := json.Unmarshal(raw, &ref); err != nil || ref.ID == "" {
		return badRequest("prompt", "invalid prompt reference: need {\"id\": ..., \"version\"|\"label\": ..., \"variables\": {...}}")
	}
	var (
		rendered *prompts.Rendered
		err      error
	)
	if k == kindChat {
		rendered, err = s.prompts.RenderChat(ref)
	} else {
		rendered, err = s.prompts.RenderText(ref)
	}
	if err != nil {
		if errors.Is(err, prompts.ErrNotFound) || errors.Is(err, prompts.ErrNoVersion) || errors.Is(err, prompts.ErrNoLabel) {
			return &apiError{status: http.StatusNotFound, typ: openai.TypeInvalidRequest, code: "prompt_not_found", prop: "prompt", msg: err.Error()}
		}
		return badRequest("prompt", "%v", err)
	}
	p.rendered = rendered

	if k == kindChat {
		delete(p.body, "prompt")
		var own []json.RawMessage
		if _, err := p.body.Get("messages", &own); err != nil {
			return badRequest("messages", "%v", err)
		}
		msgs := make([]any, 0, len(rendered.Messages)+len(own))
		for _, m := range rendered.Messages {
			msgs = append(msgs, m)
		}
		for _, m := range own {
			msgs = append(msgs, m)
		}
		_ = p.body.Set("messages", msgs)
	} else {
		_ = p.body.Set("prompt", rendered.Text)
	}
	for key, v := range rendered.Params {
		if !p.body.Has(key) {
			if err := p.body.Set(key, v); err != nil {
				return badRequest("prompt", "prompt parameter %q: %v", key, err)
			}
		}
	}
	return nil
}

// completionPrompt extracts the text of a /v1/completions prompt.
func completionPrompt(body openai.Body) (string, bool) {
	var s string
	if ok, err := body.Get("prompt", &s); ok && err == nil {
		return s, true
	}
	var list []string
	if ok, err := body.Get("prompt", &list); ok && err == nil {
		return strings.Join(list, "\n"), true
	}
	return "", false
}

func setRateLimitHeaders(h http.Header, d budget.Decision) {
	if d.Limits.TokensPerMinute > 0 && !d.Degraded {
		h.Set("X-Ratelimit-Limit-Tokens", strconv.FormatInt(d.Limits.TokensPerMinute, 10))
		h.Set("X-Ratelimit-Remaining-Tokens", strconv.FormatInt(d.MinuteRemaining, 10))
		reset := d.MinuteReset
		if !d.Allowed && d.Reason == budget.ReasonMinute && !d.TooLarge {
			reset = d.RetryAfter
		}
		h.Set("X-Ratelimit-Reset-Tokens", reset.Round(time.Millisecond).String())
	}
	if d.Limits.TokensPerDay > 0 && !d.Degraded {
		h.Set("X-Ratelimit-Limit-Tokens-Day", strconv.FormatInt(d.Limits.TokensPerDay, 10))
		h.Set("X-Ratelimit-Remaining-Tokens-Day", strconv.FormatInt(d.DayRemaining, 10))
	}
}

func writeBudgetRejection(w http.ResponseWriter, d budget.Decision, cost int64) {
	limit, unit := d.Limits.TokensPerMinute, "tokens per minute"
	if d.Reason == budget.ReasonDay {
		limit, unit = d.Limits.TokensPerDay, "tokens per day"
	}
	if d.TooLarge {
		openai.WriteError(w, http.StatusTooManyRequests, openai.TypeRateLimit, "request_too_large", "",
			fmt.Sprintf("Request too large: it may use up to %d tokens but the limit is %d %s. Reduce the prompt or max_tokens.", cost, limit, unit))
		return
	}
	secs := int64(math.Ceil(d.RetryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.FormatInt(max(secs, 1), 10))
	openai.WriteError(w, http.StatusTooManyRequests, openai.TypeRateLimit, "rate_limit_exceeded", "",
		fmt.Sprintf("Rate limit reached: limit %d %s, request needs up to %d. Try again in %s.", limit, unit, cost, d.RetryAfter.Round(time.Second)))
}
