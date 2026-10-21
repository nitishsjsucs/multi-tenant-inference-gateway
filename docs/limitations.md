# Testing status and limitations

## What was tested

| Area | How | Result |
|---|---|---|
| Go code | `go test -race ./...`; CI also runs gofmt, vet and build | passed |
| Budget Lua scripts | miniredis, and a real `redis-server` via `MTIG_TEST_REDIS_ADDR` (set in CI) | passed |
| Gateway HTTP path | `httptest` upstream: field pass-through, LoRA rewrite, prompt rendering, per-chunk flushing, usage stripping and reconciliation, 429 headers, Redis outage modes, upstream errors | passed |
| `cmd/epp` ext_proc handler | in-memory gRPC: full-duplex body buffering, model rewrite, header and metadata, trailers, subset hint, immediate errors, response pass-through | passed |
| Pickers | prefix affinity, spreading, load and KV guards, LoRA affinity, LRU eviction order | passed |
| Metrics parsing, DNS discovery, auth, prompts, routing, config | unit tests (vLLM v0 and v1 metric names) | passed |
| Mock vLLM | cache, LoRA and prefill models | passed |
| Manifests | CRs vs CRD structural schemas; embedded configs loaded by `TestDeployConfigMapsLoad` | schema only |
| OpenAI Python SDK | [`examples/python`](../examples/python) against a local gateway with `openai==3.35.1` | passed |
| Benchmark | 200 users against simulated replicas on a 4-vCPU sandbox | simulated only |

## What was not tested

| Not run | Consequence |
|---|---|
| real vLLM servers or GPUs | every performance number comes from `cmd/mockvllm` |
| Envoy or Envoy AI Gateway calling `cmd/epp` | the ext_proc exchange is only tested in-process |
| any Kubernetes cluster | manifests are a starting point |
| the Docker image | never built (no Docker daemon was available) |

## Limitations

| Area | Limitation |
|---|---|
| Simulation | `cmd/mockvllm` ignores prefill/decode interference, preemption when the KV cache is full and batch-size-dependent decode speed, among other things. |
| Token estimates | Reservations and prefix blocks use 4 characters per token. Budgets are reconciled to server-reported usage, but admission uses the estimate. |
| Prefix index | Approximate and per process: it records what this process routed, not what the server still caches. Each gateway or picker replica learns its own. |
| Failover | No retries or failover to another replica when an upstream connection fails. Endpoints leave rotation only after 3 failed metric scrapes. |
| Reload | No hot reload of config or prompts, and no Redis-backed prompt store. Prompt versions are immutable by convention only. |
| API surface | `/v1/completions` with token-ID prompts is rejected. Embeddings and the Responses API are not proxied. |
| Responses | Bodies are not rewritten: `model` in responses is the upstream (adapter) name. |
| `cmd/epp` | Doesn't watch InferencePools through the Kubernetes API, doesn't implement flow control or InferenceObjective priorities, and expects full-duplex body mode. |
