#!/usr/bin/env bash
# Runs a local stack in the foreground: Redis, two mock vLLM replicas and the
# gateway with config/gateway.yaml. Ctrl-C stops everything.
set -euo pipefail
cd "$(dirname "$0")/.."

REDIS_PORT=${REDIS_PORT:-6379}
mkdir -p bin
go build -o bin/ ./cmd/gateway ./cmd/mockvllm

pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

if ! redis-cli -p "$REDIS_PORT" ping >/dev/null 2>&1; then
  redis-server --port "$REDIS_PORT" --bind 127.0.0.1 --save "" --appendonly no >/dev/null &
  pids+=($!)
fi
for port in 8001 8002; do
  bin/mockvllm -addr "127.0.0.1:$port" -lora-adapters support-lora-v2,sql-lora-v1 &
  pids+=($!)
done
REDIS_ADDR="127.0.0.1:$REDIS_PORT" bin/gateway -config config/gateway.yaml &
pids+=($!)

echo "gateway: http://localhost:8080/v1   metrics: http://localhost:9090/metrics"
echo "try: curl -s localhost:8080/v1/models -H 'Authorization: Bearer sk-dev-acme-support'"
wait
