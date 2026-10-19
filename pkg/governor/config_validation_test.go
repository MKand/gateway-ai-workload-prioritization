package governor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_CascadeTargetValidation(t *testing.T) {
	const base = `
org_id: "org-1"
project_ids: ["p1"]
regions: ["us-central1", "europe-west1"]
models: ["gemini-3.5-flash"]
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

// TestExampleConfigLoads guards the shipped example config against drift.
func TestExampleConfigLoads(t *testing.T) {
	if _, err := LoadConfig(filepath.Join("..", "..", "config", "governor.yaml")); err != nil {
		t.Fatalf("config/governor.yaml does not load: %v", err)
	}
}
