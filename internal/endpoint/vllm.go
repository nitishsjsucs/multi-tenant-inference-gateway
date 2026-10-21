package endpoint

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// vLLM metric names (v0 and v1 engines).
const (
	metricWaiting      = "vllm:num_requests_waiting"
	metricRunning      = "vllm:num_requests_running"
	metricKVUsage      = "vllm:kv_cache_usage_perc"  // v1 engine
	metricGPUCacheUsed = "vllm:gpu_cache_usage_perc" // v0 engine
	metricLoRAInfo     = "vllm:lora_requests_info"
)

// ParseMetricFamilies decodes the Prometheus text exposition format.
func ParseMetricFamilies(r io.Reader) (map[string]*dto.MetricFamily, error) {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	return parser.TextToMetricFamilies(r)
}

// ParseVLLM extracts the scheduler state the pickers need from a vLLM
// /metrics payload.
func ParseVLLM(r io.Reader) (*Metrics, error) {
	families, err := ParseMetricFamilies(r)
	if err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}
	m := &Metrics{
		Waiting:        int(Sum(families, metricWaiting)),
		Running:        int(Sum(families, metricRunning)),
		ActiveAdapters: map[string]struct{}{},
	}
	if _, ok := families[metricKVUsage]; ok {
		m.KVCacheUsage = maxValue(families[metricKVUsage])
	} else {
		m.KVCacheUsage = maxValue(families[metricGPUCacheUsed])
	}
	if lora := latestSeries(families[metricLoRAInfo]); lora != nil {
		for _, lp := range lora.GetLabel() {
			switch lp.GetName() {
			case "running_lora_adapters", "waiting_lora_adapters":
				for _, name := range strings.Split(lp.GetValue(), ",") {
					if name = strings.TrimSpace(name); name != "" {
						m.ActiveAdapters[name] = struct{}{}
					}
				}
			case "max_lora":
				m.MaxLoRAs, _ = strconv.Atoi(lp.GetValue())
			}
		}
	}
	return m, nil
}

// Sum adds up every series of a gauge or counter family (0 if absent).
func Sum(families map[string]*dto.MetricFamily, name string) float64 {
	var total float64
	if mf, ok := families[name]; ok {
		for _, s := range mf.GetMetric() {
			total += value(s)
		}
	}
	return total
}

func maxValue(mf *dto.MetricFamily) float64 {
	var v float64
	for _, s := range mf.GetMetric() {
		v = max(v, value(s))
	}
	return v
}

// latestSeries picks the lora_requests_info series with the largest value.
// vLLM sets the gauge to the wall-clock time of the update, and stale label
// combinations can linger, so the newest one is authoritative.
func latestSeries(mf *dto.MetricFamily) *dto.Metric {
	var best *dto.Metric
	for _, s := range mf.GetMetric() {
		if best == nil || value(s) > value(best) {
			best = s
		}
	}
	return best
}

func value(m *dto.Metric) float64 {
	switch {
	case m.Gauge != nil:
		return m.GetGauge().GetValue()
	case m.Counter != nil:
		return m.GetCounter().GetValue()
	case m.Untyped != nil:
		return m.GetUntyped().GetValue()
	}
	return 0
}
