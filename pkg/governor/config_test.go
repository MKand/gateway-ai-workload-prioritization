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
org_id: "org-12345"
project_ids:
  - "proj-alpha"
regions:
  - "us-central1"
models:
  - "gemini-3.5-flash"
poll_interval: 30s
poll_timeout: 10s
safety_margin: 15
shed_threshold_best_effort: 0.75
default_org_limits:
  "us-central1/gemini-3.5-flash":
    max_rpm: 360
    max_tpm: 4000000
`
		path := writeTempYAML(t, "valid.yaml", yamlContent)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if cfg.OrgId != "org-12345" {
			t.Errorf("expected OrgId org-12345, got %s", cfg.OrgId)
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
		if cfg.ShedThresholdBestEffort != 0.75 {
			t.Errorf("expected ShedThresholdBestEffort 0.75, got %f", cfg.ShedThresholdBestEffort)
		}
		if cfg.DefaultProjectLimits == nil || cfg.DefaultOrgLimits == nil || cfg.CustomPolicies == nil {
			t.Errorf("expected maps to be non-nil")
		}
	})

	t.Run("clamping and defaults applied for out-of-range values", func(t *testing.T) {
		yamlContent := `
org_id: "org-12345"
project_ids: ["proj-1"]
regions: ["us-central1"]
models: ["gemini-3.5-flash"]
poll_interval: 5s
poll_timeout: 60s
safety_margin: 2
shed_threshold_best_effort: 0.01
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
		if cfg.ShedThresholdBestEffort != DefaultShedThresholdBestEffort {
			t.Errorf("expected ShedThresholdBestEffort defaulted to %f, got %f", DefaultShedThresholdBestEffort, cfg.ShedThresholdBestEffort)
		}
	})

	t.Run("validation errors on missing required slices", func(t *testing.T) {
		cases := []struct {
			name string
			yaml string
		}{
			{
				name: "missing org_id",
				yaml: `
org_id: ""
project_ids: ["proj-1"]
regions: ["us-central1"]
models: ["gemini-3.5-flash"]
`,
			},
			{
				name: "missing project_ids",
				yaml: `
org_id: "org-12345"
project_ids: []
regions: ["us-central1"]
models: ["gemini-3.5-flash"]
`,
			},
			{
				name: "missing regions",
				yaml: `
org_id: "org-12345"
project_ids: ["proj-1"]
regions: []
models: ["gemini-3.5-flash"]
`,
			},
			{
				name: "missing models",
				yaml: `
org_id: "org-12345"
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
