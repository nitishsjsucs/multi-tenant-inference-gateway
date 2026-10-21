// Command gateway runs the multi-tenant OpenAI-compatible inference gateway.
//
//	gateway -config config/gateway.yaml [-picker prefix-aware]
//	gateway hash-key <api-key>     # print the digest to put in api_keys
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/auth"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/budget"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/config"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/gateway"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/picker"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/prompts"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-key" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: gateway hash-key <api-key>")
			os.Exit(2)
		}
		fmt.Println(auth.HashKey(os.Args[2]))
		return
	}
	configPath := flag.String("config", "config/gateway.yaml", "path to the gateway config")
	strategy := flag.String("picker", "", "override picker.strategy ("+fmt.Sprint(picker.Strategies)+")")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(*configPath, *strategy, log); err != nil {
		log.Error("gateway failed", "err", err)
		os.Exit(1)
	}
}

func run(configPath, strategy string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if strategy != "" {
		if !picker.ValidStrategy(strategy) {
			return fmt.Errorf("unknown picker %q", strategy)
		}
		cfg.Picker.Strategy = strategy
	}
	registry, err := prompts.LoadDir(cfg.PromptsDir)
	if err != nil {
		return fmt.Errorf("load prompts: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	rdb := redis.NewClient(&redis.Options{
		Addr:                  cfg.Redis.Addr,
		Password:              cfg.Redis.Password,
		DB:                    cfg.Redis.DB,
		DialTimeout:           cfg.Redis.Timeout,
		ReadTimeout:           cfg.Redis.Timeout,
		WriteTimeout:          cfg.Redis.Timeout,
		ContextTimeoutEnabled: true,
		MaxRetries:            1,
		DialerRetries:         1, // fail fast: budget checks sit on the request path
	})
	defer rdb.Close()
	limiter := budget.NewLimiter(rdb, cfg.Redis.KeyPrefix, cfg.Budgets.FailOpen(), cfg.Redis.Timeout)

	scrapeClient := &http.Client{Timeout: 2 * time.Second}
	upstreams := map[string]gateway.Upstream{}
	for _, pc := range cfg.Pools {
		pool, err := endpoint.NewPool(endpoint.PoolOptions{
			Name: pc.Name, URLs: pc.Endpoints, DNS: pc.DNS, DNSRefresh: pc.DNSRefresh, Logger: log,
		})
		if err != nil {
			return err
		}
		if err := pool.Refresh(ctx); err != nil {
			log.Warn("initial endpoint discovery failed", "pool", pc.Name, "err", err)
		}
		pk, err := picker.New(cfg.Picker.Strategy, cfg.Picker.Prefix)
		if err != nil {
			return err
		}
		if f, ok := pk.(interface{ Forget(*endpoint.Endpoint) }); ok {
			pool.OnRemove(f.Forget)
		}
		pool.Scrape(ctx, scrapeClient)
		go pool.Run(ctx, scrapeClient, cfg.Picker.ScrapeInterval)
		upstreams[pc.Name] = gateway.Upstream{Pool: pool, Picker: pk}
	}

	srv := gateway.New(gateway.Options{
		Config: cfg, Upstreams: upstreams, Limiter: limiter, Prompts: registry, Registry: reg, Logger: log,
	})

	admin := http.NewServeMux()
	admin.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	admin.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	admin.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		for _, up := range upstreams {
			if len(up.Pool.Endpoints()) == 0 {
				http.Error(w, "pool "+up.Pool.Name()+" has no endpoints", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	servers := []*http.Server{
		{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second},
		{Addr: cfg.AdminListen, Handler: admin, ReadHeaderTimeout: 10 * time.Second},
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	log.Info("gateway listening", "api", cfg.Listen, "admin", cfg.AdminListen, "picker", cfg.Picker.Strategy,
		"budget_failure_mode", cfg.Budgets.FailureMode, "prompts_dir", cfg.PromptsDir)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return nil
}
