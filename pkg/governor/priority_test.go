package governor_test

import (
	"testing"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

func TestIsSaturated(t *testing.T) {
	tests := []struct {
		name  string
		quota *pb.ModelQuota
		want  bool
	}{
		{"nil quota fails open", nil, false},
		{"headroom left at 96% utilization", &pb.ModelQuota{MaxRpm: 1000, HeadroomRpm: 10, UtilizationRpm: 0.96}, false},
		{"rpm headroom used up", &pb.ModelQuota{MaxRpm: 1000, HeadroomRpm: 0, UtilizationRpm: 0.90}, true},
		{"tpm headroom used up", &pb.ModelQuota{MaxRpm: 1000, HeadroomRpm: 500, MaxTpm: 1000, HeadroomTpm: -5}, true},
		{"utilization at 100%", &pb.ModelQuota{UtilizationTpm: 1.0}, true},
		{"no limits means never saturated", &pb.ModelQuota{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := governor.IsSaturated(tc.quota); got != tc.want {
				t.Errorf("IsSaturated() = %v, want %v", got, tc.want)
			}
		})
	}
}
