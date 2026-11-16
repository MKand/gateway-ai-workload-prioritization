package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

type bypassClock struct{ t time.Time }

func (c *bypassClock) now() time.Time { return c.t }

// base is a minute boundary; tests read "now" 30s into minute base+5.
var base = time.Unix(1_700_000_040, 0).Truncate(time.Minute)

func minute(i int) int64 { return base.Add(time.Duration(i) * time.Minute).Unix() }

func bypassConfig() *governor.Config {
	return &governor.Config{Allowances: map[string]*governor.ModelLimit{
		"us-central1/m":   {MaxRpm: 1000, MaxTpm: 1_000_000},
		"global/tpm-only": {MaxTpm: 10_000},
	}}
}

func TestBypass_NoDataMeansNoSubtraction(t *testing.T) {
	tr := NewBypassTracker(bypassConfig(), nil)
	if e := tr.Estimate("us-central1/m"); e.RPMFactor != 1 || e.TPMFactor != 1 || e.Stale {
		t.Fatalf("before any Monitoring data: %+v, want factors 1", e)
	}
	if tr.DataAge() != -1 {
		t.Fatalf("DataAge = %v, want -1", tr.DataAge())
	}
}

func TestBypass_SameMinuteComparisonAndMargin(t *testing.T) {
	clk := &bypassClock{t: base.Add(5*time.Minute + 30*time.Second)}
	tr := NewBypassTracker(bypassConfig(), clk.now)

	// The governor admitted 500 requests in each of minutes 2-4.
	for i := 2; i <= 4; i++ {
		tr.RecordAdmitted("US-Central1/M", time.Unix(minute(i), 0).Add(20*time.Second), 500, 50_000)
	}
	// Monitoring (lagging) sees org-wide usage including bypass: 100, 300 and
	// 200 extra requests. Minute 5 is still filling up and must be ignored.
	tr.Update(UsageSeries{"us-central1/m": {
		minute(1): {Requests: 900, Tokens: 90_000}, // older than the 3-minute window
		minute(2): {Requests: 600, Tokens: 50_000},
		minute(3): {Requests: 800, Tokens: 60_000},
		minute(4): {Requests: 700, Tokens: 40_000},
		minute(5): {Requests: 5000},
	}})

	e := tr.Estimate("us-central1/m")
	if e.BypassRequests != 300 || e.BypassTokens != 10_000 {
		t.Fatalf("bypass = %d req / %d tok, want the max over minutes 2-4: 300 / 10000", e.BypassRequests, e.BypassTokens)
	}
	// factor = 1 - 10% margin - 300/1000 = 0.6; tokens: 1 - 0.1 - 0.01 = 0.89.
	if !near(e.RPMFactor, 0.6) || !near(e.TPMFactor, 0.89) {
		t.Fatalf("factors = %v / %v, want 0.6 / 0.89", e.RPMFactor, e.TPMFactor)
	}
}

func TestBypass_NoBypassStillAppliesMarginAndFloor(t *testing.T) {
	clk := &bypassClock{t: base.Add(5 * time.Minute)}
	tr := NewBypassTracker(bypassConfig(), clk.now)
	tr.RecordAdmitted("us-central1/m", time.Unix(minute(4), 0), 1000, 0)
	tr.Update(UsageSeries{"us-central1/m": {minute(4): {Requests: 900}}}) // less than admitted: no bypass
	if e := tr.Estimate("us-central1/m"); e.BypassRequests != 0 || !near(e.RPMFactor, 0.9) {
		t.Fatalf("got %+v, want bypass 0 and factor 0.9 (margin only)", e)
	}

	tr.Update(UsageSeries{"us-central1/m": {minute(4): {Requests: 50_000}}})
	if e := tr.Estimate("us-central1/m"); !near(e.RPMFactor, minAllowanceFactor) {
		t.Fatalf("huge bypass: factor %v, want floor %v", e.RPMFactor, minAllowanceFactor)
	}
	// A TPM-only allowance has no RPM limit, so its RPM factor stays 1.
	if e := tr.Estimate("global/tpm-only"); e.RPMFactor != 1 {
		t.Fatalf("unlimited dimension factor = %v, want 1", e.RPMFactor)
	}
}

func TestBypass_StaleDataFallsBackToMargin(t *testing.T) {
	clk := &bypassClock{t: base}
	tr := NewBypassTracker(bypassConfig(), clk.now)
	tr.Update(UsageSeries{})
	clk.t = clk.t.Add(bypassStaleAfter + time.Second)
	e := tr.Estimate("us-central1/m")
	if !e.Stale || !near(e.RPMFactor, 0.9) || !near(e.TPMFactor, 0.9) {
		t.Fatalf("stale: %+v, want factors 0.9", e)
	}
}

func TestBypass_CustomMarginAndUnknownTargets(t *testing.T) {
	cfg := bypassConfig()
	m := int64(0)
	cfg.BypassMarginPercent = &m
	clk := &bypassClock{t: base.Add(5 * time.Minute)}
	tr := NewBypassTracker(cfg, clk.now)
	tr.RecordAdmitted("asia-east1/m", clk.t, 10, 10) // no allowance: ignored
	tr.Update(UsageSeries{"us-central1/m": {minute(4): {Requests: 100}}})
	if e := tr.Estimate("us-central1/m"); !near(e.RPMFactor, 0.9) {
		t.Fatalf("margin 0, bypass 100/1000: factor %v, want 0.9", e.RPMFactor)
	}
	if e := tr.Estimate("asia-east1/m"); e.RPMFactor != 1 {
		t.Fatalf("unknown target factor %v, want 1", e.RPMFactor)
	}
	if got := tr.Targets(); len(got) != 2 || got[0] != "global/tpm-only" || got[1] != "us-central1/m" {
		t.Fatalf("Targets() = %v", got)
	}
}

func TestBypass_AdmittedRetention(t *testing.T) {
	tr := NewBypassTracker(bypassConfig(), nil)
	tr.RecordAdmitted("us-central1/m", time.Unix(minute(0), 0), 1, 0)
	tr.RecordAdmitted("us-central1/m", time.Unix(minute(admittedRetention+1), 0), 1, 0)
	if _, ok := tr.admitted["us-central1/m"][minute(0)]; ok {
		t.Fatal("minute older than the retention window should be dropped")
	}
}

func series(location, model string, points map[int64]int64) *monitoringpb.TimeSeries {
	ts := &monitoringpb.TimeSeries{
		Resource: &monitoredres.MonitoredResource{Labels: map[string]string{"location": location}},
		Metric:   &metric.Metric{Labels: map[string]string{"base_model": model}},
	}
	for start, v := range points {
		ts.Points = append(ts.Points, &monitoringpb.Point{
			Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(time.Unix(start, 0).Add(time.Minute))},
			Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: v}},
		})
	}
	return ts
}

func TestFetchUsageSeries_MetricsScopeAndMinuteAlignment(t *testing.T) {
	usage := &MockUsageRequestClient{ReturnValues: []TimeSeriesIterator{
		&MockTimeSeriesIterator{Items: []*monitoringpb.TimeSeries{ // requests
			series("us-central1", "m", map[int64]int64{minute(3): 40, minute(4): 50}),
			series("asia-east1", "m", map[int64]int64{minute(4): 999}), // not configured
		}},
		&MockTimeSeriesIterator{Items: []*monitoringpb.TimeSeries{ // tokens
			series("us-central1", "m", map[int64]int64{minute(4): 7000}),
		}},
	}}
	c := &GCPClient{monitoringClient: usage}
	got, err := c.FetchUsageSeries(context.Background(), "scope-project", []string{"p1", "p2"}, []string{"us-central1"}, []string{"m"})
	if err != nil {
		t.Fatal(err)
	}
	if usage.callCount != 2 {
		t.Fatalf("with a metrics scope there should be one query per metric, got %d", usage.callCount)
	}
	u := got["us-central1/m"]
	if u[minute(3)].Requests != 40 || u[minute(4)].Requests != 50 || u[minute(4)].Tokens != 7000 {
		t.Fatalf("unexpected series %v", got)
	}
	if _, ok := got["asia-east1/m"]; ok {
		t.Fatal("unconfigured targets should be dropped")
	}
}

func TestFetchUsageSeries_SumsProjectsWithoutScope(t *testing.T) {
	usage := &MockUsageRequestClient{ReturnValues: []TimeSeriesIterator{
		// Order: requests for p1, p2, then tokens for p1, p2.
		&MockTimeSeriesIterator{Items: []*monitoringpb.TimeSeries{series("us-central1", "m", map[int64]int64{minute(4): 10})}},
		&MockTimeSeriesIterator{Items: []*monitoringpb.TimeSeries{series("us-central1", "m", map[int64]int64{minute(4): 15})}},
		&MockTimeSeriesIterator{},
		&MockTimeSeriesIterator{Items: []*monitoringpb.TimeSeries{series("us-central1", "m", map[int64]int64{minute(4): 300})}},
	}}
	c := &GCPClient{monitoringClient: usage}
	got, err := c.FetchUsageSeries(context.Background(), "", []string{"p1", "p2"}, []string{"us-central1"}, []string{"m"})
	if err != nil {
		t.Fatal(err)
	}
	if usage.callCount != 4 {
		t.Fatalf("2 projects x 2 metrics = 4 queries, got %d", usage.callCount)
	}
	if u := got["us-central1/m"][minute(4)]; u.Requests != 25 || u.Tokens != 300 {
		t.Fatalf("usage should be summed across projects, got %v", got)
	}

	failing := &GCPClient{monitoringClient: &MockUsageRequestClient{Err: errors.New("denied")}}
	if _, err := failing.FetchUsageSeries(context.Background(), "", []string{"p1"}, []string{"us-central1"}, []string{"m"}); err == nil {
		t.Fatal("expected an error when Monitoring fails")
	}
}

func TestRunBypassLoop_UpdatesTracker(t *testing.T) {
	tr := NewBypassTracker(bypassConfig(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan error, 4)
	go RunBypassLoop(ctx, func(context.Context) (UsageSeries, error) {
		return UsageSeries{}, nil
	}, tr, time.Hour, func(err error) { results <- err })
	if err := <-results; err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	cancel()
	if tr.DataAge() < 0 {
		t.Fatal("tracker was not updated")
	}
}
