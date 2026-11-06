package main

import (
	"context"
	"testing"
)

func TestRunOverloadBenchmark_ZeroCriticalDrops(t *testing.T) {
	res, err := RunOverloadBenchmark(context.Background(), 50, 4)
	if err != nil {
		t.Fatalf("RunOverloadBenchmark() unexpected error: %v", err)
	}
	if res.TotalRequests != 200 {
		t.Fatalf("TotalRequests = %d, want 200", res.TotalRequests)
	}
	if res.CriticalDropped != 0 {
		t.Errorf("CriticalDropped = %d, want 0 (0.0%% drop rate)", res.CriticalDropped)
	}
	if res.BestEffortShed != res.BestEffortSent {
		t.Errorf("BestEffortShed = %d, want %d (all best_effort shed at 97%% utilization)", res.BestEffortShed, res.BestEffortSent)
	}
	if res.P50 <= 0 || res.P95 < res.P50 || res.P99 < res.P95 {
		t.Errorf("invalid latency percentiles: p50=%v p95=%v p99=%v", res.P50, res.P95, res.P99)
	}
}
