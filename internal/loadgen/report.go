package loadgen

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// AppendResult appends r as one JSON line to path.
func AppendResult(path string, r *Result) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(r)
}

// ReadResults reads a JSON-lines results file.
func ReadResults(path string) ([]Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Result
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var r Result
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// WriteTable renders results as a Markdown table.
func WriteTable(w io.Writer, results []Result) {
	fmt.Fprintln(w, "| picker | prefix-cache hit rate | TTFT p50 (ms) | TTFT p95 (ms) | TTFT p99 (ms) | req/s | output tok/s | LoRA loads | busiest replica / mean | errors |")
	fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, r := range results {
		fmt.Fprintf(w, "| %s | %.1f%% | %.0f | %.0f | %.0f | %.1f | %.0f | %.0f | %.2f | %d/%d |\n",
			r.Label, 100*r.CacheHitRate, r.TTFTp50Ms, r.TTFTp95Ms, r.TTFTp99Ms, r.ReqPerSec, r.OutTokPerSec,
			r.AdapterLoads, r.MaxMeanReplica, r.Errors, r.Requests)
	}
}
