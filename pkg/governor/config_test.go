package governor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	tempDir := t.TempDir()

	writeTempYAML := func(t *testing.T, name, content string) string {
		t.Helper()
		p := filepath.Join(tempDir, name)
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatalf("failed to write temp yaml %s: %v", name, err)
		}
		return p
	}

	t.Run("valid config with explicit fields", func(t *testing.T) {
		yamlContent := `
project_ids:
  - "proj-alpha"
regions:
  - "us-central1"
models:
  - "gemini-3.5-flash"
poll_interval: 30s
poll_timeout: 10s
safety_margin: 15
critical_reserve_percent: 25
dataplane_replicas: 3
allowances:
  "US-Central1/Gemini-3.5-Flash":
    max_rpm: 360
    max_tpm: 4000000
`
		path := writeTempYAML(t, "valid.yaml", yamlContent)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if cfg.PollInterval != 30*time.Second {
			t.Errorf("expected PollInterval 30s, got %v", cfg.PollInterval)
		}
		if cfg.PollTimeout != 10*time.Second {
			t.Errorf("expected PollTimeout 10s, got %v", cfg.PollTimeout)
		}
		if cfg.SafetyMarginPercent != 15 {
			t.Errorf("expected SafetyMarginPercent 15, got %d", cfg.SafetyMarginPercent)
		}
		if cfg.ReservePercent() != 25 || cfg.BestEffortCeiling() != 0.75 {
			t.Errorf("expected reserve 25 / ceiling 0.75, got %d / %f", cfg.ReservePercent(), cfg.BestEffortCeiling())
		}
		if cfg.Replicas() != 3 {
			t.Errorf("expected 3 replicas, got %d", cfg.Replicas())
		}
		if got := cfg.Allowances["us-central1/gemini-3.5-flash"]; got == nil || got.MaxRpm != 360 || got.MaxTpm != 4000000 {
			t.Errorf("expected allowance key normalised to lower case, got %+v", cfg.Allowances)
		}
		if cfg.DefaultProjectLimits == nil || cfg.Allowances == nil || cfg.CustomPolicies == nil {
			t.Errorf("expected maps to be non-nil")
		}
	})

	t.Run("clamping and defaults applied for out-of-range values", func(t *testing.T) {
		yamlContent := `
project_ids: ["proj-1"]
regions: ["us-central1"]
models: ["gemini-3.5-flash"]
poll_interval: 5s
poll_timeout: 60s
safety_margin: 2
allowances:
  "us-central1/gemini-3.5-flash": { max_rpm: 10 }
`
		path := writeTempYAML(t, "defaults.yaml", yamlContent)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if cfg.PollInterval != MinPollInterval {
			t.Errorf("expected PollInterval clamped to %v, got %v", MinPollInterval, cfg.PollInterval)
		}
		if cfg.PollTimeout != DefaultPollTimeout {
			t.Errorf("expected PollTimeout defaulted to %v, got %v", DefaultPollTimeout, cfg.PollTimeout)
		}
		if cfg.SafetyMarginPercent != MinSafetyMarginPercent {
			t.Errorf("expected SafetyMarginPercent clamped to %d, got %d", MinSafetyMarginPercent, cfg.SafetyMarginPercent)
		}
		if cfg.ReservePercent() != DefaultCriticalReservePercent {
			t.Errorf("expected reserve defaulted to %d, got %d", DefaultCriticalReservePercent, cfg.ReservePercent())
		}
		if cfg.Replicas() != DefaultDataplaneReplicas {
			t.Errorf("expected replicas defaulted to %d, got %d", DefaultDataplaneReplicas, cfg.Replicas())
		}
	})

	t.Run("validation errors on missing required slices", func(t *testing.T) {
		cases := []struct {
			name string
			yaml string
		}{
			{
				name: "missing allowances",
				yaml: `
project_ids: ["proj-1"]
regions: ["us-central1"]
models: ["gemini-3.5-flash"]
`,
			},
			{
				name: "missing project_ids",
				yaml: `
project_ids: []
regions: ["us-central1"]
models: ["gemini-3.5-flash"]
`,
			},
			{
				name: "missing regions",
				yaml: `
project_ids: ["proj-1"]
regions: []
models: ["gemini-3.5-flash"]
`,
			},
			{
				name: "missing models",
				yaml: `
project_ids: ["proj-1"]
regions: ["us-central1"]
models: []
`,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				path := writeTempYAML(t, tc.name+".yaml", tc.yaml)
				if _, err := LoadConfig(path); err == nil {
					t.Errorf("expected validation error for %s, got nil", tc.name)
				}
			})
		}
	})

	t.Run("missing file or invalid yaml", func(t *testing.T) {
		if _, err := LoadConfig(filepath.Join(tempDir, "nonexistent.yaml")); err == nil {
			t.Errorf("expected error for nonexistent file, got nil")
		}

		badPath := writeTempYAML(t, "bad.yaml", ":\tinvalid_yaml: [unclosed")
		if _, err := LoadConfig(badPath); err == nil {
			t.Errorf("expected error for invalid YAML, got nil")
		}
	})
}
