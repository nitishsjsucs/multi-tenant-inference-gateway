// Package routing resolves the model name a client asks for into a pool and
// an optional LoRA adapter.
package routing

import (
	"errors"
	"sort"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/config"
)

// ErrModelNotFound covers both unknown models and models the tenant may not
// use, so that model names are not disclosed across tenants.
var ErrModelNotFound = errors.New("model not found")

// Route is a resolved model.
type Route struct {
	// Requested is the name the client sent.
	Requested string
	// Pool serves the request.
	Pool string
	// Adapter is the LoRA adapter, or "" for the base model.
	Adapter string
	// Upstream is the "model" value forwarded to the model server. With vLLM
	// --enable-lora, an adapter is selected by passing its name as the model.
	Upstream string
}

// Resolver resolves model names against the configuration.
type Resolver struct {
	models map[string]config.Model
	pools  map[string]config.Pool
}

// NewResolver indexes the configured pools and models.
func NewResolver(cfg *config.Config) *Resolver {
	r := &Resolver{models: map[string]config.Model{}, pools: map[string]config.Pool{}}
	for _, p := range cfg.Pools {
		r.pools[p.Name] = p
	}
	for _, m := range cfg.Models {
		r.models[m.Name] = m
	}
	return r
}

// Resolve maps name to a route for tenant t. Lookup order: the tenant's
// aliases, then public models the tenant is allowed to use. With a nil
// tenant (the ext_proc picker, where authentication happened upstream) all
// public models resolve, and so do raw base-model and adapter names, since
// requests relayed by the gateway already carry the upstream name.
func (r *Resolver) Resolve(t *config.Tenant, name string) (Route, error) {
	if t != nil {
		if alias, ok := t.Aliases[name]; ok {
			m := r.models[alias.Model]
			adapter := alias.Adapter
			if adapter == "" {
				adapter = m.Adapter
			}
			return r.route(name, m.Pool, adapter), nil
		}
	}
	if m, ok := r.models[name]; ok && (t == nil || t.AllowsModel(name)) {
		return r.route(name, m.Pool, m.Adapter), nil
	}
	if t == nil {
		for _, p := range r.pools {
			if p.BaseModel == name {
				return r.route(name, p.Name, ""), nil
			}
			if p.HasAdapter(name) {
				return r.route(name, p.Name, name), nil
			}
		}
	}
	return Route{}, ErrModelNotFound
}

func (r *Resolver) route(requested, pool, adapter string) Route {
	upstream := r.pools[pool].BaseModel
	if adapter != "" {
		upstream = adapter
	}
	return Route{Requested: requested, Pool: pool, Adapter: adapter, Upstream: upstream}
}

// Visible lists, sorted, the model names tenant t can call.
func (r *Resolver) Visible(t *config.Tenant) []string {
	set := map[string]bool{}
	for name := range r.models {
		if t.AllowsModel(name) {
			set[name] = true
		}
	}
	for alias := range t.Aliases {
		set[alias] = true
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
