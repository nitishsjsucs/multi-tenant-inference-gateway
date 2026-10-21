package gateway

import "github.com/prometheus/client_golang/prometheus"

type metrics struct {
	requests         *prometheus.CounterVec
	tokens           *prometheus.CounterVec
	ttft             *prometheus.HistogramVec
	duration         *prometheus.HistogramVec
	inflight         prometheus.Gauge
	pickerDecisions  *prometheus.CounterVec
	prefixMatch      *prometheus.HistogramVec
	budgetRejections *prometheus.CounterVec
	budgetErrors     prometheus.Counter
	upstreamErrors   *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	latency := []float64{.01, .025, .05, .1, .2, .3, .5, .75, 1, 1.5, 2, 3, 5, 10, 20, 60}
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Completed API requests by tenant, requested model and HTTP status.",
		}, []string{"tenant", "model", "code"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_tokens_total",
			Help: "Tokens processed by tenant, requested model and type (prompt|completion).",
		}, []string{"tenant", "model", "type"}),
		ttft: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_time_to_first_token_seconds",
			Help:    "Time from request arrival to the first streamed token.",
			Buckets: latency,
		}, []string{"model", "picker"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "End-to-end request latency.",
			Buckets: latency,
		}, []string{"model", "stream"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_inflight_requests",
			Help: "Requests currently being proxied.",
		}),
		pickerDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_picker_decisions_total",
			Help: "Endpoint picker decisions by pool, picker and chosen endpoint.",
		}, []string{"pool", "picker", "endpoint"}),
		prefixMatch: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_picker_prefix_match_ratio",
			Help:    "Fraction of prompt blocks the prefix-aware picker expected to be cached on the chosen endpoint.",
			Buckets: []float64{0, .1, .2, .3, .4, .5, .6, .7, .8, .9, 1},
		}, []string{"pool"}),
		budgetRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_budget_rejections_total",
			Help: "Requests rejected by token budgets, by tenant and limit.",
		}, []string{"tenant", "reason"}),
		budgetErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_budget_store_errors_total",
			Help: "Failed calls to the Redis budget store.",
		}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_upstream_errors_total",
			Help: "Transport errors talking to model servers.",
		}, []string{"pool", "endpoint"}),
	}
	reg.MustRegister(m.requests, m.tokens, m.ttft, m.duration, m.inflight, m.pickerDecisions,
		m.prefixMatch, m.budgetRejections, m.budgetErrors, m.upstreamErrors)
	return m
}
