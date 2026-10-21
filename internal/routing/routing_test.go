package routing

import (
	"errors"
	"slices"
	"testing"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Pools: []config.Pool{{Name: "llama", BaseModel: "meta-llama/Llama-3.1-8B-Instruct", Adapters: []string{"support-lora-v2", "sql-lora-v1"}}},
		Models: []config.Model{
			{Name: "llama-3.1-8b-instruct", Pool: "llama"},
			{Name: "sql-assistant", Pool: "llama", Adapter: "sql-lora-v1"},
		},
	}
}

func TestResolve(t *testing.T) {
	r := NewResolver(testConfig())
	acme := &config.Tenant{ID: "acme", Models: []string{"llama-3.1-8b-instruct"},
		Aliases: map[string]config.Alias{
			"support-bot": {Model: "llama-3.1-8b-instruct", Adapter: "support-lora-v2"},
			"analyst":     {Model: "sql-assistant"}, // inherits the model's adapter
		}}

	cases := []struct {
		tenant   *config.Tenant
		name     string
		upstream string
		adapter  string
		err      error
	}{
		{acme, "llama-3.1-8b-instruct", "meta-llama/Llama-3.1-8B-Instruct", "", nil},
		{acme, "support-bot", "support-lora-v2", "support-lora-v2", nil},
		{acme, "analyst", "sql-lora-v1", "sql-lora-v1", nil},
		{acme, "sql-assistant", "", "", ErrModelNotFound}, // not in acme's allow-list
		{acme, "support-lora-v2", "", "", ErrModelNotFound},
		{acme, "gpt-4o", "", "", ErrModelNotFound},
		// Without a tenant (ext_proc), public names and raw upstream names resolve.
		{nil, "sql-assistant", "sql-lora-v1", "sql-lora-v1", nil},
		{nil, "support-lora-v2", "support-lora-v2", "support-lora-v2", nil},
		{nil, "meta-llama/Llama-3.1-8B-Instruct", "meta-llama/Llama-3.1-8B-Instruct", "", nil},
		{nil, "support-bot", "", "", ErrModelNotFound},
	}
	for _, tc := range cases {
		got, err := r.Resolve(tc.tenant, tc.name)
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.err)
			continue
		}
		if err == nil && (got.Upstream != tc.upstream || got.Adapter != tc.adapter || got.Pool != "llama" || got.Requested != tc.name) {
			t.Errorf("%s: route = %+v", tc.name, got)
		}
	}
}

func TestVisible(t *testing.T) {
	r := NewResolver(testConfig())
	acme := &config.Tenant{Models: []string{"llama-3.1-8b-instruct"},
		Aliases: map[string]config.Alias{"support-bot": {Model: "llama-3.1-8b-instruct"}}}
	if got, want := r.Visible(acme), []string{"llama-3.1-8b-instruct", "support-bot"}; !slices.Equal(got, want) {
		t.Errorf("Visible = %v, want %v", got, want)
	}
	all := &config.Tenant{Models: []string{"*"}}
	if got, want := r.Visible(all), []string{"llama-3.1-8b-instruct", "sql-assistant"}; !slices.Equal(got, want) {
		t.Errorf("Visible(*) = %v, want %v", got, want)
	}
}
