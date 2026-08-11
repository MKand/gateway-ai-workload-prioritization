package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"

	cloudquotas "cloud.google.com/go/cloudquotas/apiv1"
	monitoring "cloud.google.com/go/monitoring/apiv3"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	rpmQuotaID    = "GenerateContentRequestsPerMinutePerProjectPerRegionPerBaseModel"
	tpmQuotaID    = "GenerateContentInputTokensPerMinutePerRegionPerBaseModel"
	rpmMetricType = "aiplatform.googleapis.com/quota/generate_content_requests_per_minute_per_project_per_base_model/usage"
	tpmMetricType = "aiplatform.googleapis.com/quota/generate_content_input_tokens_per_minute_per_base_model/usage"
)

type GCPQuotaClient struct {
	projectLimits    map[string]governor.ModelLimit
	orgLimits        map[string]governor.ModelLimit
	quotaClient      *cloudquotas.Client
	monitoringClient *monitoring.MetricClient
	orgID            string
}

func NewGCPQuotaClient(ctx context.Context, orgID string, projectLimits, orgLimits map[string]governor.ModelLimit,
	opts ...option.ClientOption) (*GCPQuotaClient, error) {

	if orgID == "" {
		return nil, errors.New("org ID cannot be an empty string.")
	}

	if projectLimits == nil {
		return nil, errors.New("project limits map cannot be nil.")
	}
	if orgLimits == nil {
		return nil, errors.New("org limits map cannot be nil.")
	}

	quotaClient, err := cloudquotas.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create cloudquotas client: %w", err)
	}
	monitoringClient, err := monitoring.NewMetricClient(ctx, opts...)
	if err != nil {
		quotaClient.Close()
		return nil, fmt.Errorf("failed to create monitoring client: %w", err)
	}
	return &GCPQuotaClient{
		orgID:            orgID,
		projectLimits:    projectLimits,
		orgLimits:        orgLimits,
		quotaClient:      quotaClient,
		monitoringClient: monitoringClient,
	}, nil
}

func (gqc *GCPQuotaClient) fetchUsage(ctx context.Context, projectID, metricType string) (map[string]int64, error) {
	usage := make(map[string]int64)
	// 1. Define the time interval (querying the last 5 minutes to account for ingestion delay)
	now := time.Now()
	startTime := now.Add(-5 * time.Minute)

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", projectID),
		Filter: fmt.Sprintf(`metric.type = "%s" AND resource.type = "aiplatform.googleapis.com/Location"`, metricType),
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(startTime),
			EndTime:   timestamppb.New(now),
		},
		View: monitoringpb.ListTimeSeriesRequest_FULL,
		Aggregation: &monitoringpb.Aggregation{
			// Align to 1-minute windows and sum the values
			AlignmentPeriod:  durationpb.New(1 * time.Minute),
			PerSeriesAligner: monitoringpb.Aggregation_ALIGN_SUM,
			// Reduce across instances by summing them up
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			// Group by region (location) and model (base_model)
			GroupByFields: []string{
				"resource.label.location",
				"metric.label.base_model",
			},
		},
	}
	// 2. Execute the query
	it := gqc.monitoringClient.ListTimeSeries(ctx, req)
	// 3. Iterate through the aggregated timeseries results
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

		// Cloud Monitoring returns points in reverse chronological order (newest first).
		if len(resp.GetPoints()) > 0 {
			newestPoint := resp.GetPoints()[0]
			value := newestPoint.GetValue().GetInt64Value()

			key := fmt.Sprintf("%s/%s", strings.ToLower(region), strings.ToLower(model))
			usage[key] = value
		}
	}
	return usage, nil
}

func (gqc *GCPQuotaClient) fetchQuotaInfo(ctx context.Context, projectID, quotaID string) (*cloudquotaspb.QuotaInfo, error) {
	// Resource name format: projects/{project}/locations/global/services/aiplatform.googleapis.com/quotaInfos/{quotaInfo}
	resourceName := fmt.Sprintf("projects/%s/locations/global/services/aiplatform.googleapis.com/quotaInfos/%s", projectID, quotaID)
	req := &cloudquotaspb.GetQuotaInfoRequest{
		Name: resourceName,
	}
	info, err := gqc.quotaClient.GetQuotaInfo(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get quota info for %s: %w", resourceName, err)
	}
	return info, nil
}

func (gqc *GCPQuotaClient) resolveLimit(limits map[string]governor.ModelLimit, project, region, model string) governor.ModelLimit {
	modelLower := strings.ToLower(model)
	regionLower := strings.ToLower(region)
	projectLower := strings.ToLower(project)

	// 1. Try exact "project/region/model" match
	if project != "" {
		key := fmt.Sprintf("%s/%s/%s", projectLower, regionLower, modelLower)
		if limit, exists := limits[key]; exists {
			return limit
		}
	}

	// 2. Try "region/model" match (useful for Org-level limits)
	keyRM := fmt.Sprintf("%s/%s", regionLower, modelLower)
	if limit, exists := limits[keyRM]; exists {
		return limit
	}

	// 3. Try exact "model" match
	if limit, exists := limits[modelLower]; exists {
		return limit
	}

	// 4. Fallback to model family substring matching
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
	// Fallback default if no match is found
	return governor.ModelLimit{MaxRPM: 1000, MaxTPM: 2000000}
}

func extractLimits(qi *cloudquotaspb.QuotaInfo, targetRegions, targetModels []string) map[string]int64 {
	resp := make(map[string]int64)
	if qi == nil {
		return resp
	}

	for _, info := range qi.GetDimensionsInfos() {
		dims := info.GetDimensions()
		if dims == nil {
			continue // Safe fallback
		}

		region, hasRegion := dims["region"]
		model, hasModel := dims["base_model"]
		value := info.GetDetails().GetValue()

		// Helper to check if a slice contains a string (case-insensitive)
		contains := func(list []string, s string) bool {
			for _, item := range list {
				if strings.EqualFold(item, s) {
					return true
				}
			}
			return false
		}

		// 1. Resolve Regions
		var matchedRegions []string
		if hasRegion && region != "" {
			if contains(targetRegions, region) {
				matchedRegions = []string{region}
			}
		} else {
			// Wildcard: applies to all target regions
			matchedRegions = targetRegions
		}

		// 2. Resolve Models
		var matchedModels []string
		if hasModel && model != "" {
			if contains(targetModels, model) {
				matchedModels = []string{model}
			}
		} else {
			// Wildcard: applies to all target models
			matchedModels = targetModels
		}

		// 3. Populate the map for all matched combinations
		for _, r := range matchedRegions {
			for _, m := range matchedModels {
				key := fmt.Sprintf("%s/%s", strings.ToLower(r), strings.ToLower(m))
				resp[key] = value
			}
		}
	}

	return resp
}

func (gqc *GCPQuotaClient) FetchQuotas(ctx context.Context, projectIDs []string, regions []string, models []string) (map[string]*RawModelQuota, error) {

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

		rpmUsage, err := gqc.fetchUsage(ctx, p, rpmMetricType)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch RPM usage for project %s: %w", p, err)
		}

		tpmUsage, err := gqc.fetchUsage(ctx, p, tpmMetricType)
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
				MaxRPM:     orgLimit.MaxRPM,
				MaxTPM:     orgLimit.MaxTPM,
				CurrentRPM: 0,
				CurrentTPM: 0,
			}

			// 2. Add Project-level quotas (with project limits, simulated usage)
			for _, p := range projectIDs {
				data := gcpData[p]
				// Resolve RPM limit: use GCP API value if exists, else fallback to config
				key := fmt.Sprintf("%s/%s", strings.ToLower(r), strings.ToLower(m))

				var maxRPM, maxTPM, rpmUsage, tpmUsage int64
				if limit, exists := data.rpmLimits[key]; exists && limit > 0 {
					maxRPM = limit
				} else {
					maxRPM = gqc.resolveLimit(gqc.projectLimits, p, r, m).MaxRPM
				}
				// Resolve TPM limit: use GCP API value if exists, else fallback to config
				if limit, exists := data.tpmLimits[key]; exists && limit > 0 {
					maxTPM = limit
				} else {
					maxTPM = gqc.resolveLimit(gqc.projectLimits, p, r, m).MaxTPM
				}

				if usage, exists := data.rpmUsage[key]; exists {
					rpmUsage = usage
				} else {
					rpmUsage = 0 // defaulting to 0
				}

				if usage, exists := data.tpmUsage[key]; exists {
					tpmUsage = usage
				} else {
					tpmUsage = 0 // defaulting to 0
				}
				prmq := &RawModelQuota{
					ProjectID:  p,
					Region:     r,
					Model:      m,
					MaxRPM:     maxRPM,
					MaxTPM:     maxTPM,
					CurrentRPM: rpmUsage,
					CurrentTPM: tpmUsage,
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
