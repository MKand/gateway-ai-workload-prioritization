package governor_test

import (
	"testing"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

func TestNewCascadeEngine_Validation(t *testing.T) {
	t.Run("empty policy name -> error", func(t *testing.T) {
		policies := map[string]governor.CustomPolicy{
			"": {
				Cascade: []governor.CascadeStep{
					{TargetModel: "gemini-2.5-flash", TargetRegion: "us-central1"},
				},
			},
		}
		_, err := governor.NewCascadeEngine(policies)
		if err == nil {
			t.Errorf("expected error for empty policy name, got nil")
		}
	})

	t.Run("empty cascade slice -> error", func(t *testing.T) {
		policies := map[string]governor.CustomPolicy{
			"custom1": {Cascade: nil},
		}
		_, err := governor.NewCascadeEngine(policies)
		if err == nil {
			t.Errorf("expected error for empty cascade slice, got nil")
		}
	})

	t.Run("empty TargetModel in step -> error", func(t *testing.T) {
		policies := map[string]governor.CustomPolicy{
			"custom1": {
				Cascade: []governor.CascadeStep{
					{TargetModel: "", TargetRegion: "us-central1"},
				},
			},
		}
		_, err := governor.NewCascadeEngine(policies)
		if err == nil {
			t.Errorf("expected error for empty TargetModel, got nil")
		}
	})

	t.Run("empty TargetRegion in step -> error", func(t *testing.T) {
		policies := map[string]governor.CustomPolicy{
			"custom1": {
				Cascade: []governor.CascadeStep{
					{TargetModel: "gemini-2.5-flash", TargetRegion: "   "},
				},
			},
		}
		_, err := governor.NewCascadeEngine(policies)
		if err == nil {
			t.Errorf("expected error for empty TargetRegion, got nil")
		}
	})

	t.Run("valid policy -> success", func(t *testing.T) {
		policies := map[string]governor.CustomPolicy{
			"Custom_Failover": {
				Cascade: []governor.CascadeStep{
					{TargetModel: "gemini-2.5-flash", TargetRegion: "us-central1"},
				},
			},
		}
		engine, err := governor.NewCascadeEngine(policies)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if engine == nil {
			t.Fatalf("expected engine, got nil")
		}
	})
}

func TestCascadeEngine_Evaluate(t *testing.T) {
	policies := map[string]governor.CustomPolicy{
		"quality_first": {
			Cascade: []governor.CascadeStep{
				{TargetModel: "gemini-2.5-pro", TargetRegion: "us-east4"},
				{TargetModel: "gemini-2.5-flash", TargetRegion: "us-central1"},
				{TargetModel: "gemini-2.5-flash", TargetRegion: "us-east4"},
			},
		},
	}

	engine, err := governor.NewCascadeEngine(policies)
	if err != nil {
		t.Fatalf("failed to create CascadeEngine: %v", err)
	}

	t.Run("nil primary quota -> admitted optimistically (fail-open)", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{}
		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "unknown-model", pb.Priority_PRIORITY_BEST_EFFORT, "")
		if dec.IsDrop() {
			t.Errorf("expected fail-open for nil quota, got drop")
		}
	})

	t.Run("critical traffic on healthy primary -> forwarded without modification", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-2.5-pro": {
					Model:          "gemini-2.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.50,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-2.5-pro", pb.Priority_PRIORITY_CRITICAL, "")
		if dec.IsDrop() {
			t.Errorf("expected critical to be admitted, got drop")
		}
		if dec.ReplaceModel != "" || dec.ReplaceRegion != "" {
			t.Errorf("expected no modification, got model=%q region=%q", dec.ReplaceModel, dec.ReplaceRegion)
		}
	})

	t.Run("critical traffic on saturated primary -> pass-through without modification", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-2.5-pro": {
					Model:          "gemini-2.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.98,
					HeadroomRpm:    10,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-2.5-pro", pb.Priority_PRIORITY_CRITICAL, "")
		if dec.IsDrop() {
			t.Errorf("critical traffic must never be dropped, got drop")
		}
		if dec.ReplaceModel != "" || dec.ReplaceRegion != "" {
			t.Errorf("critical traffic must pass through without mutation, got model=%q region=%q", dec.ReplaceModel, dec.ReplaceRegion)
		}
	})

	t.Run("best-effort traffic on healthy primary (<70%) -> admitted", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-2.5-pro": {
					Model:          "gemini-2.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					HeadroomRpm:    400,
					UtilizationRpm: 0.60,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-2.5-pro", pb.Priority_PRIORITY_BEST_EFFORT, "")
		if dec.IsDrop() {
			t.Errorf("expected best-effort under 70%% to be admitted, got drop")
		}
	})

	t.Run("best-effort traffic above threshold (>70%) -> dropped with 429", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-2.5-pro": {
					Model:          "gemini-2.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					HeadroomRpm:    250,
					UtilizationRpm: 0.75,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-2.5-pro", pb.Priority_PRIORITY_BEST_EFFORT, "")
		if !dec.IsDrop() {
			t.Errorf("expected best-effort above 70%% to be dropped")
		}
	})

	t.Run("custom traffic on healthy primary -> admitted without cascade", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-2.5-pro": {
					Model:          "gemini-2.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					HeadroomRpm:    500,
					UtilizationRpm: 0.50,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-2.5-pro", pb.Priority_PRIORITY_CUSTOM, "quality_first")
		if dec.IsDrop() {
			t.Errorf("expected healthy custom request to be admitted, got drop")
		}
		if dec.ReplaceModel != "" || dec.ReplaceRegion != "" {
			t.Errorf("expected no fallback, got model=%q region=%q", dec.ReplaceModel, dec.ReplaceRegion)
		}
	})

	t.Run("custom traffic on saturated primary -> cascades to first healthy target", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				// Primary saturated
				"my-proj/us-central1/gemini-2.5-pro": {
					Model:          "gemini-2.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.98,
					HeadroomRpm:    5,
				},
				// Step 1: us-east4 gemini-2.5-pro also saturated
				"my-proj/us-east4/gemini-2.5-pro": {
					Model:          "gemini-2.5-pro",
					Region:         "us-east4",
					MaxRpm:         1000,
					UtilizationRpm: 0.96,
					HeadroomRpm:    10,
				},
				// Step 2: us-central1 gemini-2.5-flash HEALTHY
				"my-proj/us-central1/gemini-2.5-flash": {
					Model:          "gemini-2.5-flash",
					Region:         "us-central1",
					MaxRpm:         2000,
					UtilizationRpm: 0.30,
					HeadroomRpm:    1400,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-2.5-pro", pb.Priority_PRIORITY_CUSTOM, "Quality_First")
		if dec.IsDrop() {
			t.Errorf("expected custom cascade to succeed, got drop")
		}
		if dec.ReplaceModel != "gemini-2.5-flash" || dec.ReplaceRegion != "us-central1" {
			t.Errorf("expected fallback to gemini-2.5-flash in us-central1, got model=%q region=%q", dec.ReplaceModel, dec.ReplaceRegion)
		}
	})

	t.Run("custom traffic with nonexistent policy -> dropped", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-2.5-pro": {
					Model:          "gemini-2.5-pro",
					Region:         "us-central1",
					MaxRpm:         1000,
					UtilizationRpm: 0.98,
				},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-2.5-pro", pb.Priority_PRIORITY_CUSTOM, "nonexistent")
		if !dec.IsDrop() {
			t.Errorf("expected drop for nonexistent policy on saturated primary")
		}
	})

	t.Run("custom traffic with all cascade targets exhausted -> dropped", func(t *testing.T) {
		snapshot := &pb.QuotaSnapshot{
			ProjectQuotas: map[string]*pb.ModelQuota{
				"my-proj/us-central1/gemini-2.5-pro":   {UtilizationRpm: 0.98},
				"my-proj/us-east4/gemini-2.5-pro":      {UtilizationRpm: 0.98},
				"my-proj/us-central1/gemini-2.5-flash": {UtilizationRpm: 0.98},
				"my-proj/us-east4/gemini-2.5-flash":    {UtilizationRpm: 0.98},
			},
		}

		dec := engine.Evaluate(snapshot, "my-proj", "us-central1", "gemini-2.5-pro", pb.Priority_PRIORITY_CUSTOM, "quality_first")
		if !dec.IsDrop() {
			t.Errorf("expected drop when all cascade steps are exhausted")
		}
	})
}
