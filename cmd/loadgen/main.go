// Command loadgen benchmarks the gateway with concurrent multi-turn chats.
//
//	loadgen run -target http://localhost:8080 -metrics http://localhost:8101/metrics,... -label prefix-aware -out results.jsonl
//	loadgen report results.jsonl
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/loadgen"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "report":
		if len(os.Args) != 3 {
			usage()
		}
		results, err := loadgen.ReadResults(os.Args[2])
		if err != nil {
			fatal(err)
		}
		loadgen.WriteTable(os.Stdout, results)
	default:
		usage()
	}
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfg := loadgen.Config{}
	metrics := fs.String("metrics", "", "comma-separated model-server /metrics URLs")
	out := fs.String("out", "", "append the JSON result to this file")
	fs.StringVar(&cfg.Label, "label", "run", "label for the result row")
	fs.StringVar(&cfg.Target, "target", "http://localhost:8080", "gateway base URL")
	fs.IntVar(&cfg.Users, "users", 200, "concurrent virtual users")
	fs.DurationVar(&cfg.Warmup, "warmup", 5*time.Second, "warm-up period excluded from results")
	fs.DurationVar(&cfg.Duration, "duration", 25*time.Second, "measured period")
	fs.IntVar(&cfg.TurnsMin, "turns-min", 3, "minimum turns per conversation")
	fs.IntVar(&cfg.TurnsMax, "turns-max", 6, "maximum turns per conversation")
	fs.DurationVar(&cfg.ThinkMin, "think-min", time.Second, "minimum think time between turns")
	fs.DurationVar(&cfg.ThinkMax, "think-max", 3*time.Second, "maximum think time between turns")
	fs.Uint64Var(&cfg.Seed, "seed", 1, "random seed")
	_ = fs.Parse(args)
	if *metrics != "" {
		cfg.MetricsURLs = strings.Split(*metrics, ",")
	}
	if cfg.TurnsMax < cfg.TurnsMin || cfg.ThinkMax < cfg.ThinkMin || cfg.Users < 1 {
		fatal(fmt.Errorf("invalid turns, think time or users"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := loadgen.Run(ctx, cfg)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%-14s requests=%d errors=%d req/s=%.1f ttft_p50=%.0fms p95=%.0fms p99=%.0fms cache_hit=%.1f%% lora_loads=%.0f\n",
		res.Label, res.Requests, res.Errors, res.ReqPerSec, res.TTFTp50Ms, res.TTFTp95Ms, res.TTFTp99Ms,
		100*res.CacheHitRate, res.AdapterLoads)
	if *out != "" {
		if err := loadgen.AppendResult(*out, res); err != nil {
			fatal(err)
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: loadgen run [flags] | loadgen report <results.jsonl>")
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "loadgen:", err)
	os.Exit(1)
}
