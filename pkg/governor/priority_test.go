package governor_test

import (
	"testing"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

func TestEvaluatePriority(t *testing.T) {
	tests := []struct {
		name          string
		priority      pb.Priority
		quota         *pb.ModelQuota
		shedThreshold float64
		wantDrop      bool
		wantForward   bool
	}{
		{
			name:     "best effort under 70% threshold -> allowed",
			priority: pb.Priority_PRIORITY_BEST_EFFORT,
			quota: &pb.ModelQuota{
				MaxRpm:         1000,
				MaxTpm:         2000000,
				UtilizationRpm: 0.50,
				UtilizationTpm: 0.60,
				HeadroomRpm:    500,
				HeadroomTpm:    800000,
			},
			shedThreshold: 0.70,
			wantDrop:      false,
			wantForward:   true,
		},
		{
			name:     "best effort over 70% threshold -> shed 429",
			priority: pb.Priority_PRIORITY_BEST_EFFORT,
			quota: &pb.ModelQuota{
				MaxRpm:         1000,
				MaxTpm:         2000000,
				UtilizationRpm: 0.75,
				UtilizationTpm: 0.50,
				HeadroomRpm:    250,
				HeadroomTpm:    1000000,
			},
			shedThreshold: 0.70,
			wantDrop:      true,
			wantForward:   false,
		},
		{
			name:     "best effort with zero headroom -> shed 429",
			priority: pb.Priority_PRIORITY_BEST_EFFORT,
			quota: &pb.ModelQuota{
				MaxRpm:         1000,
				MaxTpm:         2000000,
				UtilizationRpm: 0.65,
				UtilizationTpm: 0.65,
				HeadroomRpm:    0,
				HeadroomTpm:    0,
			},
			shedThreshold: 0.70,
			wantDrop:      true,
			wantForward:   false,
		},
		{
			name:     "critical traffic under 95% -> allowed",
			priority: pb.Priority_PRIORITY_CRITICAL,
			quota: &pb.ModelQuota{
				MaxRpm:         1000,
				MaxTpm:         2000000,
				UtilizationRpm: 0.85,
				UtilizationTpm: 0.88,
				HeadroomRpm:    150,
				HeadroomTpm:    240000,
			},
			shedThreshold: 0.70,
			wantDrop:      false,
			wantForward:   true,
		},
		{
			name:     "critical traffic over 95% -> flagged for fallback",
			priority: pb.Priority_PRIORITY_CRITICAL,
			quota: &pb.ModelQuota{
				MaxRpm:         1000,
				MaxTpm:         2000000,
				UtilizationRpm: 0.96,
				UtilizationTpm: 0.90,
				HeadroomRpm:    40,
				HeadroomTpm:    200000,
			},
			shedThreshold: 0.70,
			wantDrop:      false,
			wantForward:   true, // EvaluatePriority alone flags it in reason, cascade engine handles routing
		},
		{
			name:          "nil quota -> fails open optimistically",
			priority:      pb.Priority_PRIORITY_BEST_EFFORT,
			quota:         nil,
			shedThreshold: 0.70,
			wantDrop:      false,
			wantForward:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec := governor.EvaluatePriority(tc.priority, tc.quota, tc.shedThreshold)
			if dec.IsDrop() != tc.wantDrop {
				t.Errorf("expected Drop=%v, got %v (reason: %s)", tc.wantDrop, dec.IsDrop(), dec.Reason)
			}
			if dec.IsForward() != tc.wantForward {
				t.Errorf("expected Forward=%v, got %v", tc.wantForward, dec.IsForward())
			}
		})
	}
}

func TestDefaultPriorityPolicy(t *testing.T) {
	policy := governor.NewDefaultPriorityPolicy(0.70)
	if policy.Name() != "default_priority_policy" {
		t.Errorf("expected name 'default_priority_policy', got %q", policy.Name())
	}

	saturatedQuota := &pb.ModelQuota{
		UtilizationRpm: 0.80,
	}
	dec := policy.Evaluate(saturatedQuota)
	if !dec.IsDrop() {
		t.Errorf("expected policy to drop saturated best-effort request")
	}
}
