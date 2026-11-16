package dataplane

import (
	"time"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/telemetry"
)

// Metrics are the data plane's Prometheus metrics. A nil *Metrics is valid
// and records nothing, so tests and callers can skip metrics entirely.
type Metrics struct {
	decisions      *telemetry.CounterVec
	upstream       *telemetry.CounterVec
	upstream429    *telemetry.CounterVec
	overAllowance  *telemetry.CounterVec
	tokensAdmitted *telemetry.CounterVec
}

// NewMetrics registers the data plane metrics on reg. cache and limiter may be
// nil; when set they back the snapshot-age and bucket-level gauges.
func NewMetrics(reg *telemetry.Registry, cache *SnapshotCache, limiter *governor.Limiter) *Metrics {
	m := &Metrics{
		decisions: reg.NewCounterVec("governor_decisions_total",
			"Requests decided by the governor, by effective priority and decision.", "priority", "decision"),
		upstream: reg.NewCounterVec("governor_upstream_responses_total",
			"Upstream (Vertex AI) responses for governed requests, by target and status class.", "target", "class"),
		upstream429: reg.NewCounterVec("governor_upstream_429_total",
			"HTTP 429 responses returned by Vertex AI for requests the governor admitted.", "target", "priority"),
		overAllowance: reg.NewCounterVec("governor_critical_over_allowance_total",
			"Critical requests admitted beyond the local allowance (never shed, counted as debt).", "target"),
		tokensAdmitted: reg.NewCounterVec("governor_estimated_tokens_admitted_total",
			"Estimated input tokens of admitted requests.", "target", "priority"),
	}
	if cache != nil {
		reg.NewGaugeFunc("governor_snapshot_age_seconds",
			"Seconds since the control plane last synced the quota snapshot (-1 before the first snapshot).",
			func() []telemetry.Sample {
				s := cache.Get()
				if s == nil || s.GetLastSyncedAt() == nil {
					return []telemetry.Sample{{Value: -1}}
				}
				return []telemetry.Sample{{Value: time.Since(s.GetLastSyncedAt().AsTime()).Seconds()}}
			})
	}
	if limiter != nil {
		reg.NewGaugeFunc("governor_allowance_available",
			"Requests and tokens currently available in this replica's local bucket (negative = critical debt).",
			func() []telemetry.Sample {
				var out []telemetry.Sample
				for target, a := range limiter.Available() {
					out = append(out,
						telemetry.Sample{LabelValues: []string{target, "requests"}, Value: float64(a.Requests)},
						telemetry.Sample{LabelValues: []string{target, "tokens"}, Value: float64(a.Tokens)})
				}
				return out
			}, "target", "unit")
	}
	return m
}

// observeDecision records one governor decision.
func (m *Metrics) observeDecision(r Result) {
	if m == nil {
		return
	}
	prio := r.Priority
	if prio == "" {
		prio = "none" // bypassed requests have no priority
	}
	m.decisions.Inc(prio, r.Decision)
	if r.Target == "" || r.Shed {
		return
	}
	m.tokensAdmitted.Add(r.Tokens, r.Target, r.Priority)
	if r.OverAllowance {
		m.overAllowance.Inc(r.Target)
	}
}

// observeUpstream records the upstream status for an admitted request.
func (m *Metrics) observeUpstream(target, priority string, status int) {
	if m == nil || target == "" {
		return
	}
	m.upstream.Inc(target, statusClass(status))
	if status == 429 {
		m.upstream429.Inc(target, priority)
	}
}

func statusClass(status int) string {
	switch {
	case status == 429:
		return "429"
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 200 && status < 400:
		return "2xx_3xx"
	default:
		return "other"
	}
}
