// Package gateway implements the OpenAI-compatible HTTP front end: tenant
// authentication, versioned prompts, model/LoRA routing, token budgets and
// endpoint picking, with streaming pass-through to the model servers.
package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/auth"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/budget"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/config"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/openai"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/picker"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/prompts"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/routing"
)

// Upstream is a pool of replicas and the picker that chooses among them.
type Upstream struct {
	Pool   *endpoint.Pool
	Picker picker.Picker
}

// Options wires a Server's dependencies.
type Options struct {
	Config    *config.Config
	Upstreams map[string]Upstream // keyed by pool name
	Limiter   *budget.Limiter
	Prompts   *prompts.Registry
	Client    *http.Client
	Registry  prometheus.Registerer
	Logger    *slog.Logger
}

// Server is the gateway's HTTP handler.
type Server struct {
	cfg       *config.Config
	auth      *auth.Authenticator
	resolver  *routing.Resolver
	prompts   *prompts.Registry
	limiter   *budget.Limiter
	upstreams map[string]Upstream
	client    *http.Client
	metrics   *metrics
	log       *slog.Logger
}

// New builds a Server.
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Registry == nil {
		o.Registry = prometheus.NewRegistry()
	}
	if o.Client == nil {
		o.Client = NewUpstreamClient()
	}
	s := &Server{
		cfg:       o.Config,
		auth:      auth.New(o.Config.Tenants),
		resolver:  routing.NewResolver(o.Config),
		prompts:   o.Prompts,
		limiter:   o.Limiter,
		upstreams: o.Upstreams,
		client:    o.Client,
		metrics:   newMetrics(o.Registry),
		log:       o.Logger,
	}
	if s.limiter != nil {
		s.limiter.OnFailure = func(err error) {
			s.metrics.budgetErrors.Inc()
			s.log.Warn("budget store error", "err", err)
		}
	}
	return s
}

// NewUpstreamClient returns an HTTP client tuned for many concurrent,
// long-lived streaming requests to a handful of hosts.
func NewUpstreamClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 2048
	t.MaxIdleConnsPerHost = 512
	t.IdleConnTimeout = 90 * time.Second
	t.DisableCompression = true
	return &http.Client{Transport: t}
}

// Handler returns the API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.completions(kindChat))
	mux.HandleFunc("POST /v1/completions", s.completions(kindText))
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		openai.WriteError(w, http.StatusNotFound, openai.TypeNotFound, "unknown_url",
			"", "Unknown request URL: "+r.Method+" "+r.URL.Path)
	})
	return mux
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.auth.Authenticate(r)
	if err != nil {
		openai.WriteError(w, http.StatusUnauthorized, openai.TypeAuthentication, "invalid_api_key", "", err.Error())
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	resp := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}
	for _, name := range s.resolver.Visible(tenant) {
		resp.Data = append(resp.Data, model{ID: name, Object: "model", OwnedBy: "multi-tenant-inference-gateway"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
