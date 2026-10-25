package metrics

import "github.com/prometheus/client_golang/prometheus"

// PromptCountRecorder counts the requests the stats tokenizer left on the byte estimate because the prompt count gate was full.
type PromptCountRecorder struct {
	gateFull *prometheus.CounterVec
}

func NewPromptCountRecorder(telemetry *Metrics) *PromptCountRecorder {
	recorder := &PromptCountRecorder{
		gateFull: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "devshard_gateway_prompt_count_gate_full_total",
			Help: "Requests left on the byte estimate instead of the tokenizer's count because every prompt count slot was taken, by model.",
		}, []string{"model"}),
	}
	telemetry.Register(recorder.gateFull)
	return recorder
}

func (r *PromptCountRecorder) GateFull(model string) {
	if r == nil {
		return
	}
	r.gateFull.WithLabelValues(metricLabel(model, labelUnknown)).Inc()
}
