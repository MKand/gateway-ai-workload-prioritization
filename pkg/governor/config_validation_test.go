package governor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_CascadeTargetValidation(t *testing.T) {
	const base = `
project_ids: ["p1"]
regions: ["us-central1", "europe-west1", "global"]
models: ["gemini-3.5-flash"]
allowances:
  "us-central1/gemini-3.5-flash": { max_rpm: 100, max_tpm: 100000 }
  "europe-west1/gemini-3.5-flash": { max_rpm: 100, max_tpm: 100000 }
`
	tests := []struct {
		name    string
		policy  string
		wantErr string
	}{
		{
			name: "listed region and model",
			policy: `
custom_policies:
  fallback-eu:
    cascade:
      - target_model: "gemini-3.5-flash"
        region: "europe-west1"
`,
		},
		{
			name: "case and whitespace insensitive",
			policy: `
custom_policies:
  fallback-eu:
    cascade:
      - target_model: " Gemini-3.5-Flash "
        region: "EUROPE-WEST1"
`,
		},
		{
			name: "unlisted region is rejected",
			policy: `
custom_policies:
  fallback-us:
    cascade:
      - target_model: "gemini-3.5-flash"
        region: "us-east1"
`,
			wantErr: `custom_policies.fallback-us.cascade[0] region "us-east1" is not listed in regions`,
		},
		{
			name: "unlisted model is rejected",
			policy: `
custom_policies:
  downgrade:
    cascade:
      - target_model: "gemini-3.5-flash"
        region: "us-central1"
      - target_model: "gemini-3.5-flash-lite"
        region: "us-central1"
`,
			wantErr: `custom_policies.downgrade.cascade[1] target_model "gemini-3.5-flash-lite" is not listed in models`,
		},
		{
			name: "listed target without an allowance is rejected",
			policy: `
custom_policies:
  to-global:
    cascade:
      - target_model: "gemini-3.5-flash"
        region: "global"
`,
			wantErr: `custom_policies.to-global.cascade[0] target "global/gemini-3.5-flash" has no entry in allowances`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "cfg.yaml")
			if err := os.WriteFile(p, []byte(base+tc.policy), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(p)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadConfig_AllowancesAndAdmissionSettings(t *testing.T) {
	const head = `
project_ids: ["p1"]
regions: ["us-central1", "global"]
models: ["gemini-3.5-flash"]
`
	const okAllowance = `
allowances:
  "global/gemini-3.5-flash": { max_tpm: 2000000 }
`
	tests := []struct {
		name    string
		body    string
		wantErr string
		check   func(t *testing.T, c *Config)
	}{
		{
			name: "global location and tpm-only allowance",
			body: okAllowance,
			check: func(t *testing.T, c *Config) {
				if got := c.Allowances["global/gemini-3.5-flash"]; got == nil || got.MaxTpm != 2000000 || got.MaxRpm != 0 {
					t.Errorf("allowance = %+v", got)
				}
			},
		},
		{
			name:    "key without slash",
			body:    "allowances:\n  \"gemini-3.5-flash\": { max_rpm: 10 }\n",
			wantErr: `allowances key "gemini-3.5-flash" must be "location/model"`,
		},
		{
			name:    "key with extra segment",
			body:    "allowances:\n  \"p1/global/gemini-3.5-flash\": { max_rpm: 10 }\n",
			wantErr: `must be "location/model"`,
		},
		{
			name:    "unlisted location",
			body:    "allowances:\n  \"us-east1/gemini-3.5-flash\": { max_rpm: 10 }\n",
			wantErr: `location "us-east1" is not listed in regions`,
		},
		{
			name:    "unlisted model",
			body:    "allowances:\n  \"global/gemini-9\": { max_rpm: 10 }\n",
			wantErr: `model "gemini-9" is not listed in models`,
		},
		{
			name:    "zero limits",
			body:    "allowances:\n  \"global/gemini-3.5-flash\": { max_rpm: 0, max_tpm: 0 }\n",
			wantErr: `needs max_rpm and/or max_tpm > 0`,
		},
		{
			name:    "negative limit",
			body:    "allowances:\n  \"global/gemini-3.5-flash\": { max_rpm: -1, max_tpm: 10 }\n",
			wantErr: `needs max_rpm and/or max_tpm > 0`,
		},
		{
			name:    "case-insensitive duplicate",
			body:    "allowances:\n  \"global/gemini-3.5-flash\": { max_rpm: 1 }\n  \"GLOBAL/gemini-3.5-flash\": { max_rpm: 2 }\n",
			wantErr: `is duplicated`,
		},
		{
			name: "reserve zero is allowed",
			body: okAllowance + "critical_reserve_percent: 0\n",
			check: func(t *testing.T, c *Config) {
				if c.ReservePercent() != 0 || c.BestEffortCeiling() != 1 {
					t.Errorf("reserve=%d ceiling=%f", c.ReservePercent(), c.BestEffortCeiling())
				}
			},
		},
		{
			name:    "reserve above max",
			body:    okAllowance + "critical_reserve_percent: 95\n",
			wantErr: `critical_reserve_percent 95 must be between 0 and 90`,
		},
		{
			name:    "reserve negative",
			body:    okAllowance + "critical_reserve_percent: -5\n",
			wantErr: `critical_reserve_percent -5 must be between 0 and 90`,
		},
		{
			name:    "zero replicas",
			body:    okAllowance + "dataplane_replicas: 0\n",
			wantErr: `dataplane_replicas 0 must be at least 1`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "cfg.yaml")
			if err := os.WriteFile(p, []byte(head+tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

func TestParsePriorityName(t *testing.T) {
	tests := map[string]struct {
		want Priority
		ok   bool
	}{
		"critical":    {Priority_PRIORITY_CRITICAL, true},
		" CRITICAL ":  {Priority_PRIORITY_CRITICAL, true},
		"best_effort": {Priority_PRIORITY_BEST_EFFORT, true},
		"best-effort": {Priority_PRIORITY_BEST_EFFORT, true},
		"besteffort":  {Priority_PRIORITY_BEST_EFFORT, true},
		"custom":      {Priority_PRIORITY_CUSTOM, true},
		"":            {Priority_PRIORITY_UNSPECIFIED, false},
		"urgent":      {Priority_PRIORITY_UNSPECIFIED, false},
	}
	for in, tc := range tests {
		got, ok := ParsePriorityName(in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParsePriorityName(%q) = (%v, %v), want (%v, %v)", in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestExampleConfigLoads guards the shipped example config against drift.
func TestExampleConfigLoads(t *testing.T) {
	if _, err := LoadConfig(filepath.Join("..", "..", "config", "governor.yaml")); err != nil {
		t.Fatalf("config/governor.yaml does not load: %v", err)
	}
}
