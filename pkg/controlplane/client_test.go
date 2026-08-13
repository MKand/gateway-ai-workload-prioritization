package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
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

// GCP Quota Fallback Test
// Scenario: The GCP Cloud Quotas API returns an empty response (no limits configured on GCP) or fails to find a limit.
// Expected Behavior: FetchQuotas should successfully fall back to using the static limits provided in the Config (projectLimits and orgLimits).
// Why: This verifies that your service doesn't break if the GCP API is empty, and that the hierarchical fallback config works.
// Test_FetchQuotas verifies GCPClient.FetchQuotas integration with fallback logic.
// Test_FetchMetrics verifies GCPClient.FetchMetrics integration with fallback logic.
func Test_FetchMetrics(t *testing.T) {
	ctx := context.Background()

	// 1. Initialize client with default config limits (fallbacks)
	gqc := NewMockGCPClient(
		ctx,
		map[string]pb.ModelLimit{
			"projecta/us-central1/model1": {MaxRpm: 100, MaxTpm: 5000},
			"projectb/us-central1/model1": {MaxRpm: 100, MaxTpm: 5000},
		},
		map[string]pb.ModelLimit{
			"us-central1/model1": {MaxRpm: 500, MaxTpm: 25000},
		},
	)

	tests := []struct {
		name       string
		projectIDs []string
		regions    []string
		models     []string
		// Mock configurations for this test case
		quotaInfo    *cloudquotaspb.QuotaInfo
		usageReturns []TimeSeriesIterator
		// Expectations
		want    map[string]*RawModelQuota
		wantErr string
	}{
		{
			name:       "exact_match_from_api_values", // Happy Flow
			projectIDs: []string{"projecta"},
			regions:    []string{"us-central1"},
			models:     []string{"model1"},
			quotaInfo: &cloudquotaspb.QuotaInfo{
				DimensionsInfos: []*cloudquotaspb.DimensionsInfo{
					{
						Dimensions: map[string]string{
							"region":     "us-central1",
							"base_model": "model1",
						},
						Details: &cloudquotaspb.QuotaDetails{
							Value: 150, // API limit is 150 (config is 100)
						},
					},
				},
			},
			usageReturns: []TimeSeriesIterator{
				&MockTimeSeriesIterator{
					Items: []*monitoringpb.TimeSeries{
						makeMockTimeSeries("us-central1", "model1", 80), // 80 RPM usage
					},
				},
				&MockTimeSeriesIterator{
					Items: []*monitoringpb.TimeSeries{
						makeMockTimeSeries("us-central1", "model1", 40000), // 40k TPM usage
					},
				},
			},
			want: map[string]*RawModelQuota{
				"projecta/us-central1/model1": {
					ProjectId:  "projecta",
					Region:     "us-central1",
					Model:      "model1",
					MaxRpm:     150,   // Uses API limit
					MaxTpm:     150,   // Uses API limit (returned same for both RPM/TPM mock here)
					CurrentRpm: 80,    // Uses API usage
					CurrentTpm: 40000, // Uses API usage
				},
				"us-central1/model1": {
					ProjectId:  "",
					Region:     "us-central1",
					Model:      "model1",
					MaxRpm:     500, // Org limit from config
					MaxTpm:     25000,
					CurrentRpm: 0, // Org usage starts at 0 before reconciliation aggregation
					CurrentTpm: 0,
				},
			},
		},
		{
			name:       "fallback_to_config_limits_when_api_empty", // Fallback Flow
			projectIDs: []string{"projecta"},
			regions:    []string{"us-central1"},
			models:     []string{"model1"},
			quotaInfo:  nil, // Empty API response
			usageReturns: []TimeSeriesIterator{
				&MockTimeSeriesIterator{}, // 0 RPM usage
				&MockTimeSeriesIterator{}, // 0 TPM usage
			},
			want: map[string]*RawModelQuota{
				"projecta/us-central1/model1": {
					ProjectId:  "projecta",
					Region:     "us-central1",
					Model:      "model1",
					MaxRpm:     100,  // Falls back to config limit (100)
					MaxTpm:     5000, // Falls back to config limit (5000)
					CurrentRpm: 0,
					CurrentTpm: 0,
				},
				"us-central1/model1": {
					ProjectId:  "",
					Region:     "us-central1",
					Model:      "model1",
					MaxRpm:     500, // Org limit from config
					MaxTpm:     25000,
					CurrentRpm: 0,
					CurrentTpm: 0,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Configure mocks for this test run
			mockQuota := gqc.quotaClient.(*MockQuotaRequestClient)
			mockQuota.ReturnValue = tt.quotaInfo

			mockUsage := gqc.monitoringClient.(*MockUsageRequestClient)
			mockUsage.ReturnValues = tt.usageReturns
			mockUsage.callCount = 0 // Important: reset call count for each test case

			got, err := gqc.FetchMetrics(ctx, tt.projectIDs, tt.regions, tt.models)
			if err != nil {
				if tt.wantErr == "" {
					t.Fatalf("unexpected error: %v", err)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if tt.wantErr != "" {
				t.Fatalf("expected error %q, got nil", tt.wantErr)
			}

			// Verify results
			if len(got) != len(tt.want) {
				t.Fatalf("returned map size %d, want %d", len(got), len(tt.want))
			}
			for k, wantQ := range tt.want {
				gotQ, exists := got[k]
				if !exists {
					t.Errorf("expected key %s not found in results", k)
					continue
				}
				if gotQ.MaxRpm != wantQ.MaxRpm || gotQ.MaxTpm != wantQ.MaxTpm ||
					gotQ.CurrentRpm != wantQ.CurrentRpm || gotQ.CurrentTpm != wantQ.CurrentTpm {
					t.Errorf("key %s: got %+v, want %+v", k, gotQ, wantQ)
				}
			}
		})
	}
}

func TestNewGCPClient_Validation(t *testing.T) {
	ctx := context.Background()
	dummyLimits := map[string]pb.ModelLimit{}
	dummyQuota := &MockQuotaRequestClient{}
	dummyUsage := &MockUsageRequestClient{}

	if _, err := NewGCPClient(ctx, "", dummyLimits, dummyLimits, dummyQuota, dummyUsage); err == nil {
		t.Error("Expected error for empty orgID, got nil")
	}

	if _, err := NewGCPClient(ctx, "org", nil, dummyLimits, dummyQuota, dummyUsage); err == nil {
		t.Error("Expected error for nil projectLimits, got nil")
	}

	if _, err := NewGCPClient(ctx, "org", dummyLimits, nil, dummyQuota, dummyUsage); err == nil {
		t.Error("Expected error for nil orgLimits, got nil")
	}
}

func TestRawModelQuota_GetKey_Errors(t *testing.T) {
	tests := []struct {
		name string
		rmq  *RawModelQuota
	}{
		{"missing_project", &RawModelQuota{Region: "us-central1", Model: "gemini-pro"}},
		{"missing_region", &RawModelQuota{ProjectId: "proj", Model: "gemini-pro"}},
		{"missing_model", &RawModelQuota{ProjectId: "proj", Region: "us-central1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.rmq.GetKey(); err == nil {
				t.Errorf("Expected error for mqc %+v, got nil", tt.rmq)
			}
		})
	}
}

func TestFetchMetrics_APIFailure(t *testing.T) {
	ctx := context.Background()
	gqc := NewMockGCPClient(ctx, map[string]pb.ModelLimit{}, map[string]pb.ModelLimit{})

	// Inject a hard API error
	mockQuota := gqc.quotaClient.(*MockQuotaRequestClient)
	mockQuota.Err = errors.New("API Quota Limit Exceeded")

	_, err := gqc.FetchMetrics(ctx, []string{"proj"}, []string{"us-central1"}, []string{"gemini-pro"})
	if err == nil {
		t.Fatal("Expected FetchMetrics to fail when API returns error, but it succeeded")
	}
}
