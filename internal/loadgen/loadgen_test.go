package loadgen

import (
	"bytes"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	for p, want := range map[float64]time.Duration{50: 50 * time.Millisecond, 95: 95 * time.Millisecond, 99: 99 * time.Millisecond} {
		if got := percentile(d, p); got != want {
			t.Errorf("p%v = %v, want %v", p, got, want)
		}
	}
	if percentile(nil, 50) != 0 {
		t.Error("empty input")
	}
}

func TestSummarize(t *testing.T) {
	samples := []sample{
		{ttft: 100 * time.Millisecond, e2e: time.Second, outTokens: 10},
		{ttft: 300 * time.Millisecond, e2e: time.Second, outTokens: 30},
		{err: "http_429"},
	}
	before := map[string]counters{"a": {queries: 100, hits: 10, requests: 5}, "b": {}}
	after := map[string]counters{"a": {queries: 1100, hits: 610, requests: 8, loads: 2}, "b": {queries: 1000, hits: 400, requests: 1}}
	r := summarize(Config{Label: "x", Duration: 2 * time.Second, MetricsURLs: []string{"a", "b"}}, samples, before, after)
	if r.Requests != 3 || r.Errors != 1 || r.ErrorKinds["http_429"] != 1 || r.ReqPerSec != 1 || r.OutTokPerSec != 20 {
		t.Errorf("counts: %+v", r)
	}
	if r.CacheHitRate != 0.5 || r.AdapterLoads != 2 || r.MaxMeanReplica != 1.5 {
		t.Errorf("server stats: hit %v loads %v imbalance %v", r.CacheHitRate, r.AdapterLoads, r.MaxMeanReplica)
	}

	path := filepath.Join(t.TempDir(), "r.jsonl")
	if err := AppendResult(path, r); err != nil {
		t.Fatal(err)
	}
	results, err := ReadResults(path)
	if err != nil || len(results) != 1 {
		t.Fatalf("read back: %v %v", results, err)
	}
	var buf bytes.Buffer
	WriteTable(&buf, results)
	if !strings.Contains(buf.String(), "| x | 50.0% |") {
		t.Errorf("table:\n%s", buf.String())
	}
}

func TestTenantsAreWellFormed(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	total := 0
	for _, tn := range Tenants() {
		total += tn.Weight
		if (tn.System == "") == (tn.PromptID == "") {
			t.Errorf("%s: set exactly one of System and PromptID", tn.Name)
		}
		if tn.Ask(r, 0) == "" || tn.Ask(r, 3) == "" {
			t.Errorf("%s: empty question", tn.Name)
		}
	}
	seen := map[string]int{}
	ts := Tenants()
	for range 10000 {
		seen[weighted(r, ts, total).Name]++
	}
	if seen["acme-support"] < 2700 || seen["umbrella-legal"] < 800 || seen["umbrella-legal"] > 1200 {
		t.Errorf("weights not respected: %v", seen)
	}
	if n := len(analyticsSystem()); n < 4000 {
		t.Errorf("analytics system prompt is only %d chars", n)
	}
}
