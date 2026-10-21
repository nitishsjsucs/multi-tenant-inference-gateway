GO ?= go
IMAGE ?= ghcr.io/nitishsjsucs/multi-tenant-inference-gateway:latest

.PHONY: all build test race vet fmt fmt-check check dev bench bench-lora docker clean

all: check build

build: ## Build all binaries into ./bin
	$(GO) build -o bin/ ./cmd/...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: fmt-check vet race

dev: ## Run redis, two mock vLLM replicas and the gateway locally
	scripts/dev.sh

bench: ## Compare pickers against simulated replicas (about 2.5 minutes)
	scripts/bench.sh

bench-lora: ## Same, with 3 LoRA adapters competing for 2 slots per replica
	MAX_LORAS=2 OUT=bench-results/lora-constrained scripts/bench.sh

docker:
	docker build -t $(IMAGE) .

clean:
	rm -rf bin bench-results
