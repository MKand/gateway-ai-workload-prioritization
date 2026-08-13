package controlplane

import (
	"testing"

	"cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
)

func TestExtractLimits(t *testing.T) {
	tests := []struct {
		name          string
		quotaInfo     *cloudquotaspb.QuotaInfo
		targetRegions []string
		targetModels  []string
		want          map[string]int64
	}{
		{
			name: "exact_match",
			quotaInfo: &cloudquotaspb.QuotaInfo{
				DimensionsInfos: []*cloudquotaspb.DimensionsInfo{
					{
						Dimensions: map[string]string{
							"region":     "us-central1",
							"base_model": "model1",
						},
						Details: &cloudquotaspb.QuotaDetails{
							Value: 100,
						},
					},
				},
			},
			targetRegions: []string{"us-central1"},
			targetModels:  []string{"model1"},
			want: map[string]int64{
				"us-central1/model1": 100,
			},
		},
		{
			name: "missing_region_matches_all",
			quotaInfo: &cloudquotaspb.QuotaInfo{
				DimensionsInfos: []*cloudquotaspb.DimensionsInfo{
					{
						Dimensions: map[string]string{
							"base_model": "model1",
						},
						Details: &cloudquotaspb.QuotaDetails{
							Value: 100,
						},
					},
				},
			},
			targetRegions: []string{"us-central1", "us-west1"},
			targetModels:  []string{"model1"},
			want: map[string]int64{
				"us-central1/model1": 100,
				"us-west1/model1":    100,
			},
		},
		{
			name: "filter_out_non_target",
			quotaInfo: &cloudquotaspb.QuotaInfo{
				DimensionsInfos: []*cloudquotaspb.DimensionsInfo{
					{
						Dimensions: map[string]string{
							"base_model": "model1",
							"region":     "europe-west4",
						},
						Details: &cloudquotaspb.QuotaDetails{
							Value: 100,
						},
					},
				},
			},
			targetRegions: []string{"us-central1", "us-west1"},
			targetModels:  []string{"model1"},
			want:          map[string]int64{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLimits(tt.quotaInfo, tt.targetRegions, tt.targetModels)
			if len(got) != len(tt.want) {
				t.Fatalf("extractLimits() returned map of size %d, want %d", len(got), len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("extractLimits() key %s = %d, want %d", k, got[k], v)
				}
			}
		})

	}
}

func TestGCPQuotaClient_ResolveLimit(t *testing.T) {
	gqc := &GCPClient{}
	tests := []struct {
		name    string
		limits  map[string]pb.ModelLimit
		project string
		region  string
		model   string
		want    pb.ModelLimit
	}{
		{
			name: "exact_match_project_region_model",
			limits: map[string]pb.ModelLimit{
				"projecta/us-central1/modela": {
					MaxRpm: 100,
					MaxTpm: 10000,
				},
				"projecta/us-central1/modelb": {
					MaxRpm: 70,
					MaxTpm: 70000,
				},
			},
			project: "projecta",
			region:  "us-central1",
			model:   "modela",
			want: pb.ModelLimit{
				MaxRpm: 100,
				MaxTpm: 10000,
			},
		},
		// Region/Model Match: Matches region/model when project is empty.
		{
			name: "exact_match_region_model_empty_project",
			limits: map[string]pb.ModelLimit{
				"us-central1/modela": {
					MaxRpm: 100,
					MaxTpm: 10000,
				},
				"projecta/us-central1/modelb": {
					MaxRpm: 70,
					MaxTpm: 70000,
				},
			},
			project: "",
			region:  "us-central1",
			model:   "modela",
			want: pb.ModelLimit{
				MaxRpm: 100,
				MaxTpm: 10000,
			},
		},
		//Model Match: Matches model
		{
			name: "exact_match_model",
			limits: map[string]pb.ModelLimit{
				"modela": {
					MaxRpm: 100,
					MaxTpm: 10000,
				},
				"projecta/us-central1/modelb": {
					MaxRpm: 70,
					MaxTpm: 70000,
				},
			},
			project: "",
			region:  "us-central1",
			model:   "modela",
			want: pb.ModelLimit{
				MaxRpm: 100,
				MaxTpm: 10000,
			},
		},
		// Substring Match: Matches a model family suffix.
		{
			name: "exact_match_model",
			limits: map[string]pb.ModelLimit{
				"flash-lite": {
					MaxRpm: 100,
					MaxTpm: 10000,
				},
				"flash": {
					MaxRpm: 70,
					MaxTpm: 70000,
				},
			},
			project: "abc",
			region:  "us-central1",
			model:   "modela-flash-lite",
			want: pb.ModelLimit{
				MaxRpm: 100,
				MaxTpm: 10000,
			},
		},
	}

	// Substring Match: Matches a model family suffix (e.g., gemini-1.5-flash-other falling back to flash key).
	// Global Fallback: Returns hardcoded defaults when no match is found.

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gqc.resolveLimit(tt.limits, tt.project, tt.region, tt.model)
			if got.MaxRpm != tt.want.MaxRpm || got.MaxTpm != tt.want.MaxTpm {
				t.Errorf("resolveLimit() = %+v, want RPM %d / TPM %d", got, tt.want.MaxRpm, tt.want.MaxTpm)
			}
		})
	}
}
