package config

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const digest = "47c0c7636be2dab927068d6818497303daa92c2ffe25f39c8cd88b9b4326ef5b"

const valid = `
redis: {addr: "${TEST_REDIS_ADDR:-localhost:6379}"}
pools:
  - name: llama
    base_model: meta-llama/Llama-3.1-8B-Instruct
    endpoints: [http://10.0.0.1:8000]
    adapters: [support-lora-v2]
models:
  - {name: llama-3.1-8b-instruct, pool: llama}
tenants:
  - id: acme
    api_keys: [` + digest + `]
    models: ["*"]
    aliases:
      support-bot: {model: llama-3.1-8b-instruct, adapter: support-lora-v2}
    tokens_per_minute: 1000
`

func TestParseAppliesDefaultsAndEnv(t *testing.T) {
	t.Setenv("TEST_REDIS_ADDR", "redis:6380")
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Addr != "redis:6380" {
		t.Errorf("redis addr = %q, want env value", cfg.Redis.Addr)
	}
	if cfg.Listen != ":8080" || cfg.Picker.Strategy != "prefix-aware" || cfg.Picker.ScrapeInterval != 250*time.Millisecond {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if !cfg.Budgets.FailOpen() {
		t.Error("budgets should fail open by default")
	}
	if cfg.Picker.Prefix.BlockSize != 64 {
		t.Errorf("prefix block size = %d, want 64", cfg.Picker.Prefix.BlockSize)
	}
	if !cfg.Tenants[0].AllowsModel("anything") {
		t.Error(`"*" should allow every model`)
	}
}

func TestExpandEnvDefault(t *testing.T) {
	got := string(expandEnv([]byte("a: ${MTIG_UNSET_VAR:-fallback} b: $HOME c: ${MTIG_UNSET_VAR}")))
	if got != "a: fallback b: $HOME c: " {
		t.Errorf("expandEnv = %q", got)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]struct{ from, to, wantErr string }{
		"unknown pool":         {"pool: llama}", "pool: nope}", `unknown pool "nope"`},
		"adapter not in pool":  {"adapter: support-lora-v2}", "adapter: other}", `adapter "other" is not listed`},
		"bad digest":           {digest, "sk-plaintext", "hex SHA-256"},
		"unknown tenant model": {`models: ["*"]`, `models: [no-such-model]`, `unknown model "no-such-model"`},
		"bad failure mode":     {"redis:", "budgets: {failure_mode: maybe}\nredis:", "failure_mode"},
		"bad picker":           {"redis:", "picker: {strategy: magic}\nredis:", "picker.strategy"},
		"unknown field":        {"redis:", "redsi: {}\nredis:", "field redsi not found"},
		"both endpoint kinds":  {"endpoints: [http://10.0.0.1:8000]", "endpoints: [http://10.0.0.1:8000]\n    dns: vllm:8000", "exactly one of endpoints or dns"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(valid, tc.from, tc.to, 1)
			_, err := Parse([]byte(doc))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestSampleConfigsLoad(t *testing.T) {
	t.Setenv("MOCK_ENDPOINTS", "http://127.0.0.1:8101,http://127.0.0.1:8102")
	for _, path := range []string{"../../config/gateway.yaml", "../../config/bench.yaml"} {
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

// The configs embedded in the Kubernetes manifests must stay loadable.
func TestDeployConfigMapsLoad(t *testing.T) {
	for _, path := range []string{"../../deploy/kubernetes/gateway.yaml", "../../deploy/inference-extension/epp.yaml"} {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(f)
		found := 0
		for {
			var doc struct {
				Kind string            `yaml:"kind"`
				Data map[string]string `yaml:"data"`
			}
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if doc.Kind != "ConfigMap" {
				continue
			}
			for name, body := range doc.Data {
				found++
				if _, err := Parse([]byte(body)); err != nil {
					t.Errorf("%s %s: %v", path, name, err)
				}
			}
		}
		f.Close()
		if found == 0 {
			t.Errorf("%s: no ConfigMap data found", path)
		}
	}
}
