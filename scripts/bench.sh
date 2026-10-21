#!/usr/bin/env bash
# Benchmarks each endpoint picker: for every picker it starts REPLICAS fresh
# mock vLLM replicas (cold caches) and a gateway, runs the load generator,
# and finally prints a comparison table. Requires redis-server and curl.
#
#   scripts/bench.sh                  # defaults below
#   MAX_LORAS=2 scripts/bench.sh      # 3 adapters competing for 2 slots per replica
set -euo pipefail
cd "$(dirname "$0")/.."

REPLICAS=${REPLICAS:-4}
USERS=${USERS:-200}
WARMUP=${WARMUP:-5s}
DURATION=${DURATION:-25s}
PICKERS=${PICKERS:-"random round-robin least-queue prefix-aware"}
MAX_LORAS=${MAX_LORAS:-4}    # adapters resident per replica (vLLM --max-loras)
MOCK_FLAGS=${MOCK_FLAGS:-}   # extra mockvllm flags, e.g. "-prefill-tps 20000"
SEED=${SEED:-1}
BASE_PORT=${BASE_PORT:-18100}
GATEWAY_PORT=${GATEWAY_PORT:-18080}
REDIS_PORT=${REDIS_PORT:-16390}
OUT=${OUT:-bench-results}
ADAPTERS=support-lora-v2,sql-lora-v1,legal-lora-v1

command -v redis-server >/dev/null || { echo "redis-server not found" >&2; exit 1; }
mkdir -p "$OUT" bin
go build -o bin/ ./cmd/gateway ./cmd/mockvllm ./cmd/loadgen
results="$OUT/results.jsonl"
: > "$results"

pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT

wait_http() {
  for _ in $(seq 100); do
    curl -fsS -o /dev/null "$1" 2>/dev/null && return 0
    sleep 0.1
  done
  echo "timed out waiting for $1" >&2
  return 1
}

redis-server --port "$REDIS_PORT" --bind 127.0.0.1 --save "" --appendonly no > "$OUT/redis.log" 2>&1 &
pids+=($!)

echo "replicas=$REPLICAS users=$USERS warmup=$WARMUP duration=$DURATION max_loras=$MAX_LORAS mock_flags='$MOCK_FLAGS'"
for picker in $PICKERS; do
  run_pids=()
  endpoints=()
  metrics=()
  for i in $(seq 0 $((REPLICAS - 1))); do
    port=$((BASE_PORT + i))
    # shellcheck disable=SC2086 # MOCK_FLAGS is a list of flags
    bin/mockvllm -addr "127.0.0.1:$port" -lora-adapters "$ADAPTERS" -max-loras "$MAX_LORAS" $MOCK_FLAGS > "$OUT/mock-$picker-$i.log" 2>&1 &
    run_pids+=($!)
    pids+=($!)
    endpoints+=("http://127.0.0.1:$port")
    metrics+=("http://127.0.0.1:$port/metrics")
  done
  for m in "${metrics[@]}"; do wait_http "$m"; done

  MOCK_ENDPOINTS=$(IFS=,; echo "${endpoints[*]}") \
  REDIS_ADDR="127.0.0.1:$REDIS_PORT" \
  GATEWAY_LISTEN="127.0.0.1:$GATEWAY_PORT" \
  GATEWAY_ADMIN_LISTEN="127.0.0.1:$((GATEWAY_PORT + 1))" \
    bin/gateway -config config/bench.yaml -picker "$picker" -log-level warn > "$OUT/gateway-$picker.log" 2>&1 &
  run_pids+=($!)
  pids+=($!)
  wait_http "http://127.0.0.1:$((GATEWAY_PORT + 1))/readyz"

  bin/loadgen run -target "http://127.0.0.1:$GATEWAY_PORT" -metrics "$(IFS=,; echo "${metrics[*]}")" \
    -users "$USERS" -warmup "$WARMUP" -duration "$DURATION" -seed "$SEED" -label "$picker" -out "$results"
  curl -fsS "http://127.0.0.1:$((GATEWAY_PORT + 1))/metrics" > "$OUT/gateway-metrics-$picker.txt"

  kill "${run_pids[@]}" 2>/dev/null || true
  wait "${run_pids[@]}" 2>/dev/null || true
done

echo
bin/loadgen report "$results" | tee "$OUT/table.md"
