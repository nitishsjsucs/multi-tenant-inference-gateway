// Package config loads and validates the gateway configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/picker"
)

// Config is the root of gateway.yaml.
type Config struct {
	// Listen is the address of the OpenAI-compatible API.
	Listen string `yaml:"listen"`
	// AdminListen serves /metrics, /healthz and /readyz.
	AdminListen string `yaml:"admin_listen"`
	// PromptsDir holds the versioned prompt registry (<id>.yaml files).
	PromptsDir string `yaml:"prompts_dir"`
	// UpstreamTimeout bounds a whole upstream request, including streaming.
	UpstreamTimeout time.Duration `yaml:"upstream_timeout"`

	Redis   Redis    `yaml:"redis"`
	Budgets Budgets  `yaml:"budgets"`
	Picker  Picker   `yaml:"picker"`
	Pools   []Pool   `yaml:"pools"`
	Models  []Model  `yaml:"models"`
	Tenants []Tenant `yaml:"tenants"`
}

// Redis configures the budget store.
type Redis struct {
	Addr      string        `yaml:"addr"`
	Password  string        `yaml:"password"`
	DB        int           `yaml:"db"`
	KeyPrefix string        `yaml:"key_prefix"`
	Timeout   time.Duration `yaml:"timeout"`
}

// Budgets configures token budget enforcement.
type Budgets struct {
	// FailureMode decides what happens when Redis is unreachable:
	// "open" admits the request without accounting, "closed" rejects it with 503.
	FailureMode string `yaml:"failure_mode"`
	// DefaultMaxTokens is reserved for completions when a request sets no max_tokens.
	DefaultMaxTokens int `yaml:"default_max_tokens"`
}

// FailOpen reports whether requests are admitted while Redis is down.
func (b Budgets) FailOpen() bool { return b.FailureMode != "closed" }

// Picker selects and tunes the endpoint picker.
type Picker struct {
	Strategy       string              `yaml:"strategy"`
	ScrapeInterval time.Duration       `yaml:"scrape_interval"`
	Prefix         picker.PrefixConfig `yaml:"prefix"`
}

// Pool is a set of interchangeable model-server replicas serving one base model.
type Pool struct {
	Name string `yaml:"name"`
	// BaseModel is the name the model server serves the base weights under.
	BaseModel string `yaml:"base_model"`
	// Endpoints are static base URLs such as http://10.0.0.7:8000.
	Endpoints []string `yaml:"endpoints"`
	// DNS is an alternative to Endpoints: a host:port (typically a headless
	// Service) whose A records are re-resolved every DNSRefresh.
	DNS        string        `yaml:"dns"`
	DNSRefresh time.Duration `yaml:"dns_refresh"`
	// Adapters lists the LoRA adapter names the replicas can serve.
	Adapters []string `yaml:"adapters"`
}

// HasAdapter reports whether the pool serves the named LoRA adapter.
func (p Pool) HasAdapter(name string) bool {
	for _, a := range p.Adapters {
		if a == name {
			return true
		}
	}
	return false
}

// Model is a public model name exposed to every tenant that is allowed to use it.
type Model struct {
	Name    string `yaml:"name"`
	Pool    string `yaml:"pool"`
	Adapter string `yaml:"adapter"`
}

// Tenant is a product team with its own keys, model access and budgets.
type Tenant struct {
	ID string `yaml:"id"`
	// APIKeys holds hex-encoded SHA-256 digests of the tenant's API keys.
	APIKeys []string `yaml:"api_keys"`
	// Models lists the public models the tenant may call; "*" allows all.
	Models []string `yaml:"models"`
	// Aliases are tenant-private model names, e.g. support-bot -> base + LoRA.
	Aliases map[string]Alias `yaml:"aliases"`
	// TokensPerMinute and TokensPerDay are budgets; 0 disables that limit.
	TokensPerMinute int64 `yaml:"tokens_per_minute"`
	TokensPerDay    int64 `yaml:"tokens_per_day"`
}

// Alias maps a tenant-private model name onto a public model and optional adapter.
type Alias struct {
	Model   string `yaml:"model"`
	Adapter string `yaml:"adapter"`
}

// AllowsModel reports whether the tenant may call the public model name.
func (t *Tenant) AllowsModel(name string) bool {
	for _, m := range t.Models {
		if m == "*" || m == name {
			return true
		}
	}
	return false
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default}; any other "$" is left alone.
func expandEnv(data []byte) []byte {
	return envRef.ReplaceAllFunc(data, func(m []byte) []byte {
		sub := envRef.FindSubmatch(m)
		if v, ok := os.LookupEnv(string(sub[1])); ok && v != "" {
			return []byte(v)
		}
		return sub[3]
	})
}

// Load reads, expands, decodes, defaults and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes a config document. Unknown fields are rejected.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(expandEnv(data)))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.AdminListen == "" {
		c.AdminListen = ":9090"
	}
	if c.UpstreamTimeout == 0 {
		c.UpstreamTimeout = 10 * time.Minute
	}
	if c.Redis.KeyPrefix == "" {
		c.Redis.KeyPrefix = "mtig"
	}
	if c.Redis.Timeout == 0 {
		c.Redis.Timeout = 100 * time.Millisecond
	}
	if c.Budgets.FailureMode == "" {
		c.Budgets.FailureMode = "open"
	}
	if c.Budgets.DefaultMaxTokens == 0 {
		c.Budgets.DefaultMaxTokens = 1024
	}
	if c.Picker.Strategy == "" {
		c.Picker.Strategy = picker.StrategyPrefixAware
	}
	if c.Picker.ScrapeInterval == 0 {
		c.Picker.ScrapeInterval = 250 * time.Millisecond
	}
	c.Picker.Prefix = c.Picker.Prefix.WithDefaults()
	for i := range c.Pools {
		if c.Pools[i].DNSRefresh == 0 {
			c.Pools[i].DNSRefresh = 10 * time.Second
		}
	}
}

// Validate checks cross references between pools, models and tenants.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Budgets.FailureMode != "open" && c.Budgets.FailureMode != "closed" {
		fail("budgets.failure_mode must be \"open\" or \"closed\", got %q", c.Budgets.FailureMode)
	}
	if !picker.ValidStrategy(c.Picker.Strategy) {
		fail("picker.strategy %q is not one of %s", c.Picker.Strategy, strings.Join(picker.Strategies, ", "))
	}

	pools := make(map[string]Pool, len(c.Pools))
	for i, p := range c.Pools {
		switch {
		case p.Name == "":
			fail("pools[%d]: name is required", i)
		case pools[p.Name].Name != "":
			fail("pools[%d]: duplicate pool %q", i, p.Name)
		}
		if p.BaseModel == "" {
			fail("pool %q: base_model is required", p.Name)
		}
		if (len(p.Endpoints) == 0) == (p.DNS == "") {
			fail("pool %q: set exactly one of endpoints or dns", p.Name)
		}
		pools[p.Name] = p
	}

	models := make(map[string]Model, len(c.Models))
	for i, m := range c.Models {
		if m.Name == "" {
			fail("models[%d]: name is required", i)
			continue
		}
		if _, dup := models[m.Name]; dup {
			fail("models[%d]: duplicate model %q", i, m.Name)
		}
		p, ok := pools[m.Pool]
		if !ok {
			fail("model %q: unknown pool %q", m.Name, m.Pool)
		} else if m.Adapter != "" && !p.HasAdapter(m.Adapter) {
			fail("model %q: adapter %q is not listed in pool %q", m.Name, m.Adapter, m.Pool)
		}
		models[m.Name] = m
	}

	tenantIDs := map[string]bool{}
	keyOwner := map[string]string{}
	for i, t := range c.Tenants {
		if t.ID == "" {
			fail("tenants[%d]: id is required", i)
			continue
		}
		if tenantIDs[t.ID] {
			fail("tenants[%d]: duplicate tenant %q", i, t.ID)
		}
		tenantIDs[t.ID] = true
		if len(t.APIKeys) == 0 {
			fail("tenant %q: at least one api_keys entry is required", t.ID)
		}
		for _, k := range t.APIKeys {
			if !isSHA256Hex(k) {
				fail("tenant %q: api_keys must be hex SHA-256 digests (use `gateway hash-key`)", t.ID)
				continue
			}
			if owner, dup := keyOwner[strings.ToLower(k)]; dup {
				fail("tenant %q: api key digest already used by tenant %q", t.ID, owner)
			}
			keyOwner[strings.ToLower(k)] = t.ID
		}
		for _, name := range t.Models {
			if _, ok := models[name]; !ok && name != "*" {
				fail("tenant %q: unknown model %q", t.ID, name)
			}
		}
		for alias, a := range t.Aliases {
			m, ok := models[a.Model]
			if !ok {
				fail("tenant %q alias %q: unknown model %q", t.ID, alias, a.Model)
				continue
			}
			if a.Adapter != "" && !pools[m.Pool].HasAdapter(a.Adapter) {
				fail("tenant %q alias %q: adapter %q is not listed in pool %q", t.ID, alias, a.Adapter, m.Pool)
			}
		}
		if t.TokensPerMinute < 0 || t.TokensPerDay < 0 {
			fail("tenant %q: budgets must be >= 0", t.ID)
		}
	}
	return errors.Join(errs...)
}

// Pool returns the named pool.
func (c *Config) Pool(name string) (Pool, bool) {
	for _, p := range c.Pools {
		if p.Name == name {
			return p, true
		}
	}
	return Pool{}, false
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}
