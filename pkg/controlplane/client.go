package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cloudquotas "cloud.google.com/go/cloudquotas/apiv1"
	"cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	monitoring "cloud.google.com/go/monitoring/apiv3"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	rpmQuotaID                    = "GenerateContentRequestsPerMinutePerProjectPerRegionPerBaseModel"
	tpmQuotaID                    = "GenerateContentInputTokensPerMinutePerRegionPerBaseModel"
	rpmMetricType                 = "aiplatform.googleapis.com/quota/generate_content_requests_per_minute_per_project_per_base_model/usage"
	tpmMetricType                 = "aiplatform.googleapis.com/quota/generate_content_input_tokens_per_minute_per_base_model/usage"
	MetricLookupWindowMinutes int = 5
	FallbackMaxRPM                = 1000
	FallbackMaxTPM                = 2000000
)

// ============================================================================
// Interfaces
// ============================================================================

// QuotaClient is the top-level interface for fetching quota and usage data.
type QuotaClient interface {
	FetchQuotas(ctx context.Context, projectIDs []string, regions []string, models []string) (map[string]*RawModelQuota, error)
}

// QuotaRequestClient wraps the GCP Cloud Quotas API.
type QuotaRequestClient interface {
	makeQuotaRequest(ctx context.Context, req *cloudquotaspb.GetQuotaInfoRequest) (*cloudquotaspb.QuotaInfo, error)
}

// TimeSeriesIterator interface allows mocking of the Cloud Monitoring iterator.
type TimeSeriesIterator interface {
	Next() (*monitoringpb.TimeSeries, error)
}

// UsageRequestClient wraps the GCP Cloud Monitoring API.
type UsageRequestClient interface {
	makeUsageRequest(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) (TimeSeriesIterator, error)
}

// ============================================================================
// Types
// ============================================================================

type RawModelQuota struct {
	ProjectId  string `json:"projectId"`
	Model      string `json:"model"`
	Region     string `json:"region"`
	MaxTpm     int64  `json:"maxTpm"`
	MaxRpm     int64  `json:"maxRpm"`
	CurrentRpm int64  `json:"currentRpm"`
	CurrentTpm int64  `json:"currentTpm"`
}

func (mqc *RawModelQuota) GetKey() (string, error) {
	if mqc.ProjectId == "" || mqc.Region == "" || mqc.Model == "" {
		return "", fmt.Errorf("need string value for project=%s, region=%s and model=%s", mqc.ProjectId, mqc.Region, mqc.Model)
	}
	return fmt.Sprintf("%s/%s/%s", strings.ToLower(mqc.ProjectId), strings.ToLower(mqc.Region), strings.ToLower(mqc.Model)), nil
}

// ============================================================================
// Production GCP Implementation
// ============================================================================

type GCPClient struct {
	projectLimits    map[string]pb.ModelLimit
	orgLimits        map[string]pb.ModelLimit
	quotaClient      QuotaRequestClient
	monitoringClient UsageRequestClient
	orgID            string
}

func NewGCPClient(ctx context.Context, orgID string, projectLimits, orgLimits map[string]pb.ModelLimit,
	quotaClient QuotaRequestClient, monitoringClient UsageRequestClient) (*GCPClient, error) {

	if orgID == "" {
		return nil, errors.New("org ID cannot be an empty string.")
	}

	if projectLimits == nil {
		return nil, errors.New("project limits map cannot be nil.")
	}
	if orgLimits == nil {
		return nil, errors.New("org limits map cannot be nil.")
	}
	return &GCPClient{
		orgID:            orgID,
		projectLimits:    projectLimits,
		orgLimits:        orgLimits,
		quotaClient:      quotaClient,
		monitoringClient: monitoringClient,
	}, nil
}

type GCPQuotaRequestClient struct {
	cloudQuotasClient *cloudquotas.Client
}

func NewGCPQuotaRequestClient(ctx context.Context, opts ...option.ClientOption) (*GCPQuotaRequestClient, error) {
	quotaClient, err := cloudquotas.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create cloudquotas client: %w", err)
	}

	return &GCPQuotaRequestClient{
		cloudQuotasClient: quotaClient,
	}, nil
}

func (c *GCPQuotaRequestClient) makeQuotaRequest(ctx context.Context, req *cloudquotaspb.GetQuotaInfoRequest) (*cloudquotaspb.QuotaInfo, error) {
	if c.cloudQuotasClient == nil {
		return nil, fmt.Errorf("cannot retreive quota info with a nil quota request client")
	}
	resp, err := c.cloudQuotasClient.GetQuotaInfo(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

type GCPUsageRequestClient struct {
	monitoringMetricClient *monitoring.MetricClient
}

func NewGCPUsageRequestClient(ctx context.Context, opts ...option.ClientOption) (*GCPUsageRequestClient, error) {
	monitoringClient, err := monitoring.NewMetricClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create monitoring client: %w", err)
	}
	return &GCPUsageRequestClient{
		monitoringMetricClient: monitoringClient,
	}, err
}

func (c *GCPUsageRequestClient) makeUsageRequest(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) (TimeSeriesIterator, error) {
	if c.monitoringMetricClient == nil {
		return nil, fmt.Errorf("cannot retreive metrics with a nil monitoring metric client")
	}
	return c.monitoringMetricClient.ListTimeSeries(ctx, req), nil
}

func (gqc *GCPClient) FetchQuotas(ctx context.Context, projectIDs []string, regions []string, models []string) (map[string]*RawModelQuota, error) {
	resp := map[string]*RawModelQuota{}

	type projectData struct {
		rpmLimits map[string]int64 // key: region/model
		tpmLimits map[string]int64 // key: region/model
		rpmUsage  map[string]int64 // key: region/model
		tpmUsage  map[string]int64 // key: region/model
	}
	gcpData := make(map[string]projectData) // key: projectID

	for _, p := range projectIDs {
		rawRPM, err := gqc.fetchQuotaInfo(ctx, p, rpmQuotaID)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch RPM limits for project %s: %w", p, err)
		}

		rawTPM, err := gqc.fetchQuotaInfo(ctx, p, tpmQuotaID)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch TPM limits for project %s: %w", p, err)
		}

		rpmUsage, err := gqc.fetchUsageInfo(ctx, p, rpmMetricType)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch RPM usage for project %s: %w", p, err)
		}

		tpmUsage, err := gqc.fetchUsageInfo(ctx, p, tpmMetricType)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch TPM usage for project %s: %w", p, err)
		}

		gcpData[p] = projectData{
			rpmLimits: extractLimits(rawRPM, regions, models),
			tpmLimits: extractLimits(rawTPM, regions, models),
			rpmUsage:  rpmUsage,
			tpmUsage:  tpmUsage,
		}
	}

	for _, r := range regions {
		for _, m := range models {
			// 1. Add Org-level quota (with org limits, 0 usage)
			orgLimit := gqc.resolveLimit(gqc.orgLimits, "", r, m)
			oKey := fmt.Sprintf("%s/%s", strings.ToLower(r), strings.ToLower(m))
			resp[oKey] = &RawModelQuota{
				Region:     r,
				Model:      m,
				MaxRpm:     orgLimit.MaxRpm,
				MaxTpm:     orgLimit.MaxTpm,
				CurrentRpm: 0,
				CurrentTpm: 0,
			}

			// 2. Add Project-level quotas (with project limits, simulated usage)
			for _, p := range projectIDs {
				data := gcpData[p]
				key := fmt.Sprintf("%s/%s", strings.ToLower(r), strings.ToLower(m))

				var maxRpm, maxTpm, rpmUsage, tpmUsage int64
				if limit, exists := data.rpmLimits[key]; exists && limit > 0 {
					maxRpm = limit
				} else {
					maxRpm = gqc.resolveLimit(gqc.projectLimits, p, r, m).MaxRpm
				}
				if limit, exists := data.tpmLimits[key]; exists && limit > 0 {
					maxTpm = limit
				} else {
					maxTpm = gqc.resolveLimit(gqc.projectLimits, p, r, m).MaxTpm
				}

				if usage, exists := data.rpmUsage[key]; exists {
					rpmUsage = usage
				} else {
					rpmUsage = 0
				}

				if usage, exists := data.tpmUsage[key]; exists {
					tpmUsage = usage
				} else {
					tpmUsage = 0
				}
				prmq := &RawModelQuota{
					ProjectId:  p,
					Region:     r,
					Model:      m,
					MaxRpm:     maxRpm,
					MaxTpm:     maxTpm,
					CurrentRpm: rpmUsage,
					CurrentTpm: tpmUsage,
				}
				pKey, err := prmq.GetKey()
				if err != nil {
					return nil, err
				}
				resp[pKey] = prmq
			}
		}
	}
	return resp, nil
}

func (gqc *GCPClient) fetchUsageInfo(ctx context.Context, projectID, metricType string) (map[string]int64, error) {
	usage := make(map[string]int64)
	now := time.Now()
	startTime := now.Add(time.Duration(-MetricLookupWindowMinutes) * time.Minute)

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", projectID),
		Filter: fmt.Sprintf(`metric.type = "%s" AND resource.type = "aiplatform.googleapis.com/Location"`, metricType),
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(startTime),
			EndTime:   timestamppb.New(now),
		},
		View: monitoringpb.ListTimeSeriesRequest_FULL,
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    durationpb.New(1 * time.Minute),
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_SUM,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			GroupByFields: []string{
				"resource.label.location",
				"metric.label.base_model",
			},
		},
	}
	it, err := gqc.monitoringClient.makeUsageRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("Cannot retrive usage metrics. %w", err)
	}
	for {
		resp, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to iterate timeseries for project %s: %w", projectID, err)
		}
		region := resp.GetResource().GetLabels()["location"]
		model := resp.GetMetric().GetLabels()["base_model"]

		if len(resp.GetPoints()) > 0 {
			newestPoint := resp.GetPoints()[0]
			value := newestPoint.GetValue().GetInt64Value()

			key := fmt.Sprintf("%s/%s", strings.ToLower(region), strings.ToLower(model))
			usage[key] = value
		}
	}
	return usage, nil
}

func (gqc *GCPClient) fetchQuotaInfo(ctx context.Context, projectID, quotaID string) (*cloudquotaspb.QuotaInfo, error) {
	resourceName := fmt.Sprintf("projects/%s/locations/global/services/aiplatform.googleapis.com/quotaInfos/%s", projectID, quotaID)
	req := &cloudquotaspb.GetQuotaInfoRequest{
		Name: resourceName,
	}
	info, err := gqc.quotaClient.makeQuotaRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get quota info for %s: %w", resourceName, err)
	}
	return info, nil
}

func (gqc *GCPClient) resolveLimit(limits map[string]pb.ModelLimit, project, region, model string) pb.ModelLimit {
	modelLower := strings.ToLower(model)
	regionLower := strings.ToLower(region)
	projectLower := strings.ToLower(project)

	if project != "" {
		key := fmt.Sprintf("%s/%s/%s", projectLower, regionLower, modelLower)
		if limit, exists := limits[key]; exists {
			return limit
		}
	}
	keyRM := fmt.Sprintf("%s/%s", regionLower, modelLower)
	if limit, exists := limits[keyRM]; exists {
		return limit
	}
	if limit, exists := limits[modelLower]; exists {
		return limit
	}
	if strings.Contains(modelLower, "flash-lite") {
		if limit, exists := limits["flash-lite"]; exists {
			return limit
		}
	}
	if strings.Contains(modelLower, "flash") {
		if limit, exists := limits["flash"]; exists {
			return limit
		}
	}
	if strings.Contains(modelLower, "pro") {
		if limit, exists := limits["pro"]; exists {
			return limit
		}
	}
	return pb.ModelLimit{MaxRpm: FallbackMaxRPM, MaxTpm: FallbackMaxTPM}
}

func extractLimits(qi *cloudquotaspb.QuotaInfo, targetRegions, targetModels []string) map[string]int64 {
	resp := make(map[string]int64)
	if qi == nil {
		return resp
	}
	for _, info := range qi.GetDimensionsInfos() {
		dims := info.GetDimensions()
		if dims == nil {
			continue
		}
		region, hasRegion := dims["region"]
		model, hasModel := dims["base_model"]
		value := info.GetDetails().GetValue()

		contains := func(list []string, s string) bool {
			for _, item := range list {
				if strings.EqualFold(item, s) {
					return true
				}
			}
			return false
		}

		var matchedRegions []string
		if hasRegion && region != "" {
			if contains(targetRegions, region) {
				matchedRegions = []string{region}
			}
		} else {
			matchedRegions = targetRegions
		}

		var matchedModels []string
		if hasModel && model != "" {
			if contains(targetModels, model) {
				matchedModels = []string{model}
			}
		} else {
			matchedModels = targetModels
		}

		for _, r := range matchedRegions {
			for _, m := range matchedModels {
				key := fmt.Sprintf("%s/%s", strings.ToLower(r), strings.ToLower(m))
				resp[key] = value
			}
		}
	}
	return resp
}

// ============================================================================
// Test Mocks
// ============================================================================

type MockQuotaRequestClient struct {
	ReturnValue *cloudquotaspb.QuotaInfo
	Err         error
}

func (c *MockQuotaRequestClient) makeQuotaRequest(ctx context.Context, req *cloudquotaspb.GetQuotaInfoRequest) (*cloudquotaspb.QuotaInfo, error) {
	return c.ReturnValue, c.Err
}

type MockTimeSeriesIterator struct {
	Items []*monitoringpb.TimeSeries
	Index int
	Err   error
}

func (m *MockTimeSeriesIterator) Next() (*monitoringpb.TimeSeries, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	if m.Index >= len(m.Items) {
		return nil, iterator.Done
	}
	item := m.Items[m.Index]
	m.Index++
	return item, nil
}

type MockUsageRequestClient struct {
	ReturnValues []TimeSeriesIterator
	callCount    int
	Err          error
}

func (c *MockUsageRequestClient) makeUsageRequest(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) (TimeSeriesIterator, error) {
	if c.Err != nil {
		return nil, c.Err
	}
	if c.callCount >= len(c.ReturnValues) {
		return &MockTimeSeriesIterator{}, nil
	}
	val := c.ReturnValues[c.callCount]
	c.callCount++
	return val, nil
}

func NewMockGCPClient(ctx context.Context, projectLimits, orgLimits map[string]pb.ModelLimit) *GCPClient {
	return &GCPClient{
		orgID:            "mockorgid",
		projectLimits:    projectLimits,
		orgLimits:        orgLimits,
		quotaClient:      &MockQuotaRequestClient{},
		monitoringClient: &MockUsageRequestClient{},
	}
}
