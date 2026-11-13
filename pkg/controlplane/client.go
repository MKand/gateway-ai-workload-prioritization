package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"

	cloudquotas "cloud.google.com/go/cloudquotas/apiv1"
	"cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	monitoring "cloud.google.com/go/monitoring/apiv3"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
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

// MetricsClient is the top-level interface for fetching quota and usage data.
type MetricsClient interface {
	FetchMetrics(ctx context.Context, projectIDs []string, regions []string, models []string) (map[string]*RawModelQuota, error)
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

type GCPClient struct {
	projectLimits map[string]*governor.ModelLimit
	// allowances is the configured org DSQ allowance per "location/model".
	// It becomes the org-level quota in each snapshot.
	allowances       map[string]*governor.ModelLimit
	quotaClient      QuotaRequestClient
	monitoringClient UsageRequestClient
}

func NewGCPClient(ctx context.Context, projectLimits, allowances map[string]*governor.ModelLimit,
	quotaClient QuotaRequestClient, monitoringClient UsageRequestClient) (*GCPClient, error) {

	if projectLimits == nil {
		return nil, errors.New("project limits map cannot be nil.")
	}
	if allowances == nil {
		return nil, errors.New("allowances map cannot be nil.")
	}
	return &GCPClient{
		projectLimits:    projectLimits,
		allowances:       allowances,
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

func (c *GCPQuotaRequestClient) Close() error {
	if c == nil || c.cloudQuotasClient == nil {
		return nil
	}
	return c.cloudQuotasClient.Close()
}

func (c *GCPUsageRequestClient) Close() error {
	if c == nil || c.monitoringMetricClient == nil {
		return nil
	}
	return c.monitoringMetricClient.Close()
}
func (c *GCPUsageRequestClient) makeUsageRequest(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) (TimeSeriesIterator, error) {
	if c.monitoringMetricClient == nil {
		return nil, fmt.Errorf("cannot retreive metrics with a nil monitoring metric client")
	}
	return c.monitoringMetricClient.ListTimeSeries(ctx, req), nil
}

func (gqc *GCPClient) FetchMetrics(ctx context.Context, projectIDs []string, regions []string, models []string) (map[string]*RawModelQuota, error) {
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
			log.Printf("Warning: failed to fetch RPM limits for project %s: %v. Falling back to default limits.", p, err)
			rawRPM = nil
		}

		rawTPM, err := gqc.fetchQuotaInfo(ctx, p, tpmQuotaID)
		if err != nil {
			log.Printf("Warning: failed to fetch TPM limits for project %s: %v. Falling back to default limits.", p, err)
			rawTPM = nil
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
			// 1. Add Org-level quota (the configured allowance, 0 usage; the
			// reconciler sums project usage into it)
			orgLimit := gqc.resolveFallbackLimit(gqc.allowances, "", r, m)
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
				maxRpm = resolveLimit(data.rpmLimits, key, gqc.resolveFallbackLimit(gqc.projectLimits, p, r, m).MaxRpm)
				maxTpm = resolveLimit(data.tpmLimits, key, gqc.resolveFallbackLimit(gqc.projectLimits, p, r, m).MaxTpm)

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

// resolveLimit returns the limit reported by Cloud Quotas for key. An
// unlimited quota (-1) is returned as 0, which the rest of the system treats
// as "no limit" (utilization 0, never saturated). Missing or zero values fall
// back to the configured default.
func resolveLimit(limits map[string]int64, key string, fallback int64) int64 {
	limit, exists := limits[key]
	switch {
	case exists && limit == UnlimitedQuota:
		return 0
	case exists && limit > 0:
		return limit
	default:
		return fallback
	}
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

func (gqc *GCPClient) resolveFallbackLimit(limits map[string]*governor.ModelLimit, project, region, model string) *governor.ModelLimit {
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
	return &governor.ModelLimit{MaxRpm: FallbackMaxRPM, MaxTpm: FallbackMaxTPM}
}

// UnlimitedQuota is the value Cloud Quotas uses for "no limit". extractLimits
// preserves it so callers can tell "unlimited" apart from "unknown".
const UnlimitedQuota int64 = -1

// extractLimits maps each targeted "region/model" key to its limit.
//
// A QuotaInfo mixes entries of different specificity, e.g. a region-wide
// default ({region}) and a model override ({region, base_model}). The most
// specific matching entry wins regardless of order:
// region+model > model-only > region-only > global.
// Entries without a value carry no information and are skipped.
func extractLimits(qi *cloudquotaspb.QuotaInfo, targetRegions, targetModels []string) map[string]int64 {
	resp := make(map[string]int64)
	specificity := make(map[string]int)
	if qi == nil {
		return resp
	}

	contains := func(list []string, s string) bool {
		for _, item := range list {
			if strings.EqualFold(item, s) {
				return true
			}
		}
		return false
	}

	for _, info := range qi.GetDimensionsInfos() {
		if info.GetDetails() == nil {
			continue // no value reported for this dimension set
		}
		value := info.GetDetails().GetValue()
		dims := info.GetDimensions()
		region, model := dims["region"], dims["base_model"]

		score := 0
		matchedRegions := targetRegions
		if region != "" {
			if !contains(targetRegions, region) {
				continue
			}
			matchedRegions = []string{region}
			score++
		}
		matchedModels := targetModels
		if model != "" {
			if !contains(targetModels, model) {
				continue
			}
			matchedModels = []string{model}
			score += 2
		}

		for _, r := range matchedRegions {
			for _, m := range matchedModels {
				key := fmt.Sprintf("%s/%s", strings.ToLower(r), strings.ToLower(m))
				if prev, seen := specificity[key]; seen && prev > score {
					continue
				}
				specificity[key] = score
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

func NewMockGCPClient(ctx context.Context, projectLimits, allowances map[string]*governor.ModelLimit) *GCPClient {
	return &GCPClient{
		projectLimits:    projectLimits,
		allowances:       allowances,
		quotaClient:      &MockQuotaRequestClient{},
		monitoringClient: &MockUsageRequestClient{},
	}
}

// usageSeriesWindow is how far back FetchUsageSeries reads. It must cover
// Cloud Monitoring's lag plus the bypass comparison window.
const usageSeriesWindow = 10 * time.Minute

// FetchUsageSeries reads org-wide per-minute usage (requests and input
// tokens) for the configured locations and models. With a metrics scope
// project it makes one query per metric that covers every project in the
// scope, including projects whose traffic bypasses the governor; otherwise
// it sums over projectIDs.
func (gqc *GCPClient) FetchUsageSeries(ctx context.Context, scopeProject string, projectIDs, regions, models []string) (UsageSeries, error) {
	want := map[string]bool{}
	for _, r := range regions {
		for _, m := range models {
			want[strings.ToLower(r)+"/"+strings.ToLower(m)] = true
		}
	}
	names := projectIDs
	if scopeProject != "" {
		names = []string{scopeProject}
	}

	out := UsageSeries{}
	for _, metric := range []struct {
		typ string
		add func(u *MinuteUsage, v int64)
	}{
		{rpmMetricType, func(u *MinuteUsage, v int64) { u.Requests += v }},
		{tpmMetricType, func(u *MinuteUsage, v int64) { u.Tokens += v }},
	} {
		for _, name := range names {
			if err := gqc.addUsageSeries(ctx, name, metric.typ, want, out, metric.add); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (gqc *GCPClient) addUsageSeries(ctx context.Context, project, metricType string, want map[string]bool, out UsageSeries, add func(*MinuteUsage, int64)) error {
	now := time.Now()
	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", project),
		Filter: fmt.Sprintf(`metric.type = "%s" AND resource.type = "aiplatform.googleapis.com/Location"`, metricType),
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(now.Add(-usageSeriesWindow)),
			EndTime:   timestamppb.New(now),
		},
		View: monitoringpb.ListTimeSeriesRequest_FULL,
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    durationpb.New(time.Minute),
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_SUM,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			GroupByFields:      []string{"resource.label.location", "metric.label.base_model"},
		},
	}
	it, err := gqc.monitoringClient.makeUsageRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("usage series for %s: %w", project, err)
	}
	for {
		ts, err := it.Next()
		if err == iterator.Done {
			return nil
		}
		if err != nil {
			return fmt.Errorf("usage series for %s: %w", project, err)
		}
		key := strings.ToLower(ts.GetResource().GetLabels()["location"]) + "/" + strings.ToLower(ts.GetMetric().GetLabels()["base_model"])
		if !want[key] {
			continue
		}
		if out[key] == nil {
			out[key] = map[int64]MinuteUsage{}
		}
		for _, p := range ts.GetPoints() {
			end := p.GetInterval().GetEndTime().AsTime()
			// An aligned point covers the minute that ends at its end time.
			minute := end.Add(-time.Minute).Truncate(time.Minute).Unix()
			u := out[key][minute]
			add(&u, p.GetValue().GetInt64Value())
			out[key][minute] = u
		}
	}
}

// UsageSeriesFetcher reads org-wide per-minute usage.
type UsageSeriesFetcher func(ctx context.Context) (UsageSeries, error)

// RunBypassLoop refreshes the tracker from Cloud Monitoring every interval
// until ctx is cancelled. Failures are logged; the tracker falls back to
// "allowance minus margin" once its data is older than 5 minutes.
func RunBypassLoop(ctx context.Context, fetch UsageSeriesFetcher, tracker *BypassTracker, interval time.Duration, onResult func(error)) {
	refresh := func() {
		callCtx, cancel := context.WithTimeout(ctx, interval)
		defer cancel()
		series, err := fetch(callCtx)
		if err == nil {
			tracker.Update(series)
		} else if ctx.Err() == nil {
			slog.Warn("bypass usage read failed", "err", err)
		}
		if onResult != nil {
			onResult(err)
		}
	}
	refresh()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}
