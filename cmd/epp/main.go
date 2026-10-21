// Command epp is an Envoy ext_proc endpoint picker for the Gateway API
// Inference Extension. It picks a replica of one pool from the gateway
// config using the same pickers as the standalone gateway.
//
//	epp -config config/gateway.yaml -pool llama-3-1-8b
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/config"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/extproc"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/picker"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/routing"
)

func main() {
	configPath := flag.String("config", "config/gateway.yaml", "path to the gateway config (pools, models, picker)")
	poolName := flag.String("pool", "", "pool to pick endpoints from (default: the first pool)")
	strategy := flag.String("picker", "", "override picker.strategy")
	grpcAddr := flag.String("grpc-addr", ":9002", "ext_proc gRPC listen address")
	healthAddr := flag.String("grpc-health-addr", ":9003", "gRPC health-check listen address")
	metricsAddr := flag.String("metrics-addr", ":9090", "Prometheus metrics listen address")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(*configPath, *poolName, *strategy, *grpcAddr, *healthAddr, *metricsAddr, log); err != nil {
		log.Error("epp failed", "err", err)
		os.Exit(1)
	}
}

func run(configPath, poolName, strategy, grpcAddr, healthAddr, metricsAddr string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if len(cfg.Pools) == 0 {
		return errors.New("config defines no pools")
	}
	if poolName == "" {
		poolName = cfg.Pools[0].Name
	}
	pc, ok := cfg.Pool(poolName)
	if !ok {
		return fmt.Errorf("unknown pool %q", poolName)
	}
	if strategy != "" {
		cfg.Picker.Strategy = strategy
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := endpoint.NewPool(endpoint.PoolOptions{
		Name: pc.Name, URLs: pc.Endpoints, DNS: pc.DNS, DNSRefresh: pc.DNSRefresh, Logger: log,
	})
	if err != nil {
		return err
	}
	if err := pool.Refresh(ctx); err != nil {
		log.Warn("initial endpoint discovery failed", "err", err)
	}
	pk, err := picker.New(cfg.Picker.Strategy, cfg.Picker.Prefix)
	if err != nil {
		return err
	}
	if f, ok := pk.(interface{ Forget(*endpoint.Endpoint) }); ok {
		pool.OnRemove(f.Forget)
	}
	scrapeClient := &http.Client{Timeout: 2 * time.Second}
	go pool.Run(ctx, scrapeClient, cfg.Picker.ScrapeInterval)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	srv := extproc.New(pool, pk, routing.NewResolver(cfg), reg, log)

	grpcServer := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(grpcServer, srv)

	// Health on a separate port, as the reference endpoint picker does, with
	// the service names probes commonly use.
	healthServer := grpc.NewServer()
	hs := health.NewServer()
	for _, svc := range []string{"", "liveness", "readiness", "inference-extension", extprocv3.ExternalProcessor_ServiceDesc.ServiceName} {
		hs.SetServingStatus(svc, healthpb.HealthCheckResponse_SERVING)
	}
	healthpb.RegisterHealthServer(healthServer, hs)

	metricsServer := &http.Server{Addr: metricsAddr, Handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 3)
	serveGRPC := func(s *grpc.Server, addr string) {
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			errc <- err
			return
		}
		errc <- s.Serve(lis)
	}
	go serveGRPC(grpcServer, grpcAddr)
	go serveGRPC(healthServer, healthAddr)
	go func() {
		if err := metricsServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	log.Info("endpoint picker listening", "grpc", grpcAddr, "health", healthAddr, "metrics", metricsAddr,
		"pool", pc.Name, "picker", pk.Name(), "endpoints", len(pool.Endpoints()))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	hs.Shutdown()
	grpcServer.GracefulStop()
	healthServer.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return metricsServer.Shutdown(shutdownCtx)
}
