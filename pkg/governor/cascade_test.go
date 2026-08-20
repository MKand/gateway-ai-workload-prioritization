package governor_test

import (
	"testing"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

func TestCascadeEngine(t *testing.T) {
	engine := governor.NewCascadeEngine(nil, nil)

	t.Run("primary model healthy -> forwarded without fallback", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-1.5-pro": {
					Model:          "gemini-1.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					MaxTpm:         2000000,
					UtilizationRpm: 0.50,
					UtilizationTpm: 0.50,
					HeadroomRpm:    500,
					HeadroomTpm:    1000000,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-1.5-pro", pb.Priority_PRIORITY_CRITICAL)
		if dec.IsDrop() {
			t.Errorf("expected healthy request to be admitted, got drop")
		}
		if dec.ReplaceModel != "" || dec.ReplaceRegion != "" {
			t.Errorf("expected no fallback, got model=%q region=%q", dec.ReplaceModel, dec.ReplaceRegion)
		}
	})

	t.Run("best-effort traffic on saturated model -> dropped (no fallback)", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-1.5-pro": {
					Model:          "gemini-1.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.98,
					HeadroomRpm:    10,
				},
				"my-proj/us-central1/gemini-1.5-flash": {
					Model:          "gemini-1.5-flash",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.20,
					HeadroomRpm:    800,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-1.5-pro", pb.Priority_PRIORITY_BEST_EFFORT)
		if !dec.IsDrop() {
			t.Errorf("expected best-effort to be dropped, got forwarded")
		}
		if dec.ReplaceModel != "" {
			t.Errorf("best-effort must not trigger model fallback, got %q", dec.ReplaceModel)
		}
	})

	t.Run("critical traffic on saturated primary -> model fallback in same region", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-1.5-pro": {
					Model:          "gemini-1.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.98,
					HeadroomRpm:    10,
				},
				"my-proj/us-central1/gemini-1.5-flash": {
					Model:          "gemini-1.5-flash",
					Region:         "us-central1",
					MaxRpm:         1000,
					MaxTpm:         2000000,
					UtilizationRpm: 0.30,
					UtilizationTpm: 0.30,
					HeadroomRpm:    700,
					HeadroomTpm:    1400000,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-1.5-pro", pb.Priority_PRIORITY_CRITICAL)
		if dec.IsDrop() {
			t.Errorf("expected critical request to fallback, not drop")
		}
		if dec.ReplaceModel != "gemini-1.5-flash" {
			t.Errorf("expected fallback to 'gemini-1.5-flash', got %q", dec.ReplaceModel)
		}
		if dec.ReplaceRegion != "" {
			t.Errorf("expected region to remain us-central1, got %q", dec.ReplaceRegion)
		}
	})

	t.Run("critical traffic on all models saturated in region -> regional failover", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				// Saturated in us-central1
				"my-proj/us-central1/gemini-1.5-pro": {
					Model:          "gemini-1.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.99,
					HeadroomRpm:    5,
				},
				"my-proj/us-central1/gemini-1.5-flash": {
					Model:          "gemini-1.5-flash",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.99,
					HeadroomRpm:    5,
				},
				"my-proj/us-central1/gemini-1.5-flash-8b": {
					Model:          "gemini-1.5-flash-8b",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.99,
					HeadroomRpm:    5,
				},
				"my-proj/us-central1/gemini-1.0-pro": {
					Model:          "gemini-1.0-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.99,
					HeadroomRpm:    5,
				},
				// Healthy in us-east4
				"my-proj/us-east4/gemini-1.5-pro": {
					Model:          "gemini-1.5-pro",
					Region:         "us-east4",
					MaxRpm:         1000,
					MaxTpm:         2000000,
					UtilizationRpm: 0.40,
					UtilizationTpm: 0.40,
					HeadroomRpm:    600,
					HeadroomTpm:    1200000,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-1.5-pro", pb.Priority_PRIORITY_CRITICAL)
		if dec.IsDrop() {
			t.Errorf("expected regional failover, got drop")
		}
		if dec.ReplaceRegion != "us-east4" {
			t.Errorf("expected regional failover to 'us-east4', got %q", dec.ReplaceRegion)
		}
	})

	t.Run("all models and regions exhausted -> drop as last resort", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-1.5-pro": {
					MaxRpm:         1000,
					UtilizationRpm: 0.99,
				},
				"my-proj/us-central1/gemini-1.5-flash": {
					MaxRpm:         1000,
					UtilizationRpm: 0.99,
				},
				"my-proj/us-east4/gemini-1.5-pro": {
					MaxRpm:         1000,
					UtilizationRpm: 0.99,
				},
				"my-proj/us-west1/gemini-1.5-pro": {
					MaxRpm:         1000,
					UtilizationRpm: 0.99,
				},
				"my-proj/us-east1/gemini-1.5-pro": {
					MaxRpm:         1000,
					UtilizationRpm: 0.99,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-1.5-pro", pb.Priority_PRIORITY_CRITICAL)
		if !dec.IsDrop() {
			t.Errorf("expected drop when all candidates are exhausted")
		}
	})
}
