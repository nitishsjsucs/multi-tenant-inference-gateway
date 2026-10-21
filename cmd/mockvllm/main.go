// Command mockvllm runs one simulated vLLM replica (see internal/mockvllm).
//
//	mockvllm -addr :8001 -lora-adapters support-lora-v2,sql-lora-v1
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/mockvllm"
)

func main() {
	cfg := mockvllm.DefaultConfig()
	addr := flag.String("addr", ":8001", "listen address")
	adapters := flag.String("lora-adapters", "", "comma-separated LoRA adapter names to serve")
	flag.StringVar(&cfg.Model, "model", cfg.Model, "served base model name")
	flag.IntVar(&cfg.MaxLoRAs, "max-loras", cfg.MaxLoRAs, "adapters resident at once (vLLM --max-loras)")
	flag.IntVar(&cfg.MaxNumSeqs, "max-num-seqs", cfg.MaxNumSeqs, "concurrent sequences (vLLM --max-num-seqs)")
	flag.IntVar(&cfg.BlockTokens, "block-size", cfg.BlockTokens, "tokens per KV block")
	flag.IntVar(&cfg.CharsPerToken, "chars-per-token", cfg.CharsPerToken, "characters per simulated token")
	flag.IntVar(&cfg.CacheBlocks, "cache-blocks", cfg.CacheBlocks, "KV-cache capacity in blocks")
	flag.Float64Var(&cfg.PrefillTPS, "prefill-tps", cfg.PrefillTPS, "prefill throughput, tokens/s per replica")
	flag.Float64Var(&cfg.DecodeTPS, "decode-tps", cfg.DecodeTPS, "decode speed, tokens/s per sequence")
	flag.DurationVar(&cfg.BaseLatency, "base-latency", cfg.BaseLatency, "fixed latency added to time to first token")
	flag.DurationVar(&cfg.LoRALoadLatency, "lora-load-latency", cfg.LoRALoadLatency, "penalty for loading an adapter")
	flag.IntVar(&cfg.DefaultMaxTokens, "default-max-tokens", cfg.DefaultMaxTokens, "max_tokens when a request sets none")
	flag.Parse()
	if *adapters != "" {
		cfg.Adapters = strings.Split(*adapters, ",")
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	srv := &http.Server{Addr: *addr, Handler: mockvllm.New(cfg).Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("mock vLLM listening", "addr", *addr, "model", cfg.Model, "adapters", cfg.Adapters,
		"max_num_seqs", cfg.MaxNumSeqs, "cache_blocks", cfg.CacheBlocks, "prefill_tps", cfg.PrefillTPS, "decode_tps", cfg.DecodeTPS)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
