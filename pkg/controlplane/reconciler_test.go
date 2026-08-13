package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
)

func makeMockTimeSeries(region, model string, value int64) *monitoringpb.TimeSeries {
	return &monitoringpb.TimeSeries{
		Resource: &monitoredres.MonitoredResource{
			Labels: map[string]string{
				"location": region,
			},
		},
		Metric: &metric.Metric{
			Labels: map[string]string{
				"base_model": model,
			},
		},
		Points: []*monitoringpb.Point{
			{
				Value: &monitoringpb.TypedValue{
					Value: &monitoringpb.TypedValue_Int64Value{
						Int64Value: value,
					},
				},
			},
		},
	}
}

// GetTestReconciler helper accepts the config and store from the test
func GetTestReconciler(cfg *governor.Config, store SnapshotStore) (*Reconciler, *GCPClient) {
	ctx := context.Background()
	gcpClient := NewMockGCPClient(ctx, cfg.DefaultProjectLimits, cfg.DefaultOrgLimits)
	reconciler, err := NewReconciler(cfg, gcpClient, store)
	if err != nil {
		panic(err)
	}
	return reconciler, gcpClient
}

func TestReconcile_AggregatesAndCalculates(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	cfg := &governor.Config{
		ProjectIDs:   []string{"projecta", "projectb"},
		Regions:      []string{"us-central1", "us-west1"},
		Models:       []string{"model1_flash", "model1_pro"},
		SafetyMargin: 0.3,
		DefaultProjectLimits: map[string]pb.ModelLimit{
			"projecta/us-central1/model1_pro":   {MaxRpm: 100, MaxTpm: 5000},
			"projecta/us-west1/model1_pro":      {MaxRpm: 100, MaxTpm: 5000},
			"projecta/us-central1/model1_flash": {MaxRpm: 50, MaxTpm: 2500},
			"projecta/us-west1/model1_flash":    {MaxRpm: 50, MaxTpm: 2500},
			"projectb/us-central1/model1_flash": {MaxRpm: 50, MaxTpm: 2500},
			"projectb/us-west1/model1_flash":    {MaxRpm: 50, MaxTpm: 2500},
		},
		DefaultOrgLimits: map[string]pb.ModelLimit{
			"us-central1/model1_pro":   {MaxRpm: 500, MaxTpm: 25000},
			"us-west1/model1_pro":      {MaxRpm: 500, MaxTpm: 25000},
			"us-central1/model1_flash": {MaxRpm: 200, MaxTpm: 10000},
			"us-west1/model1_flash":    {MaxRpm: 200, MaxTpm: 10000},
		},
	}

	reconciler, gcpClient := GetTestReconciler(cfg, store)

	// Helper to create iterator for a project and value
	makeIterator := func(value int64) TimeSeriesIterator {
		var items []*monitoringpb.TimeSeries
		for _, r := range cfg.Regions {
			for _, m := range cfg.Models {
				items = append(items, makeMockTimeSeries(r, m, value))
			}
		}
		return &MockTimeSeriesIterator{Items: items}
	}

	mockUsage := gcpClient.monitoringClient.(*MockUsageRequestClient)
	mockUsage.ReturnValues = []TimeSeriesIterator{
		makeIterator(100),   // projecta RPM
		makeIterator(50000), // projecta TPM
		makeIterator(100),   // projectb RPM
		makeIterator(50000), // projectb TPM
	}

	err := reconciler.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile() failed: %v", err)
	}

	snapshot, err := store.Get(ctx)
	if err != nil {
		t.Fatalf("Failed to retrieve snapshot: %v", err)
	}

	// 1. Assert Org-level quota (Aggregated usage, static config limits)
	orgQuota, err := snapshot.GetOrgQuota("us-central1", "model1_pro")
	if err != nil {
		t.Errorf("Failed to find org quota: %v", err)
	} else {
		// Org limit is static from config
		if orgQuota.MaxRpm != 500 || orgQuota.MaxTpm != 25000 {
			t.Errorf("Expected org limit 500 RPM / 25000 TPM, got %d RPM / %d TPM", orgQuota.MaxRpm, orgQuota.MaxTpm)
		}
		// Org usage is sum of projects (100 RPM + 100 RPM = 200)
		if orgQuota.CurrentRpm != 200 || orgQuota.CurrentTpm != 100000 {
			t.Errorf("Expected org usage 200 RPM / 100000 TPM, got %d RPM / %d TPM", orgQuota.CurrentRpm, orgQuota.CurrentTpm)
		}
		// Org headroom: Usable = 500 * (1 - 0.3) = 350. Headroom = 350 - 200 = 150.
		if orgQuota.HeadroomRpm != 150 {
			t.Errorf("Expected org RPM headroom 150, got %d", orgQuota.HeadroomRpm)
		}
	}

	// 2. Assert Project-level quota (Static limits, mock usage, individual math)
	p1, err := snapshot.GetProjectQuota("projecta", "us-central1", "model1_pro")
	if err != nil {
		t.Errorf("Failed to find project quota: %v", err)
	} else {
		if p1.MaxRpm != 100 || p1.MaxTpm != 5000 {
			t.Errorf("Expected project limit 100 RPM / 5000 TPM, got %d RPM / %d TPM", p1.MaxRpm, p1.MaxTpm)
		}
		if p1.HeadroomRpm != -30 {
			t.Errorf("Expected project RPM headroom -30, got %d", p1.HeadroomRpm)
		}
	}
}

type SpyStore struct {
	InMemoryStore
	saveCount int
	mu        sync.Mutex
}

func (s *SpyStore) Save(ctx context.Context, snapshot *pb.QuotaSnapshot) error {
	s.mu.Lock()
	s.saveCount++
	s.mu.Unlock()
	return s.InMemoryStore.Save(ctx, snapshot)
}

func (s *SpyStore) GetSaveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCount
}

func TestReconciler_LifecycleStart(t *testing.T) {
	spyStore := &SpyStore{}

	cfg := &governor.Config{
		ProjectIDs:   []string{"projecta"},
		Regions:      []string{"us-central1"},
		Models:       []string{"model1_pro"},
		PollInterval: 10 * time.Millisecond,
		SafetyMargin: 0.1,
		DefaultProjectLimits: map[string]pb.ModelLimit{
			"projecta/us-central1/model1_pro": {MaxRpm: 100, MaxTpm: 5000},
		},
		DefaultOrgLimits: map[string]pb.ModelLimit{
			"us-central1/model1_pro": {MaxRpm: 500, MaxTpm: 25000},
		},
	}

	reconciler, _ := GetTestReconciler(cfg, spyStore)
	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		errChan <- reconciler.Start(ctx)
	}()

	time.Sleep(25 * time.Millisecond)
	cancel()

	err := <-errChan
	if err != nil && err != context.Canceled {
		t.Errorf("Expected context.Canceled error, got: %v", err)
	}

	count := spyStore.GetSaveCount()
	if count < 3 {
		t.Errorf("Expected at least 3 reconciliation runs, but got %d", count)
	}
}

func TestReconcile_ZeroQuotaSafety(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	// 1. Setup config with MaxRPM and MaxTPM configured as 0
	cfg := &governor.Config{
		ProjectIDs:   []string{"projecta"},
		Regions:      []string{"us-central1"},
		Models:       []string{"model1_pro"},
		SafetyMargin: 0.3, // 30% margin
		DefaultProjectLimits: map[string]pb.ModelLimit{
			// Max limit is 0
			"projecta/us-central1/model1_pro": {MaxRpm: 0, MaxTpm: 0},
		},
		DefaultOrgLimits: map[string]pb.ModelLimit{
			// Org limit is also 0
			"us-central1/model1_pro": {MaxRpm: 0, MaxTpm: 0},
		},
	}

	// 2. Setup Reconciler
	reconciler, gcpClient := GetTestReconciler(cfg, store)

	makeIterator := func(value int64) TimeSeriesIterator {
		return &MockTimeSeriesIterator{
			Items: []*monitoringpb.TimeSeries{
				makeMockTimeSeries("us-central1", "model1_pro", value),
			},
		}
	}
	mockUsage := gcpClient.monitoringClient.(*MockUsageRequestClient)
	mockUsage.ReturnValues = []TimeSeriesIterator{
		makeIterator(100), // projecta RPM
		makeIterator(0),   // projecta TPM
	}

	// 3. Run reconciliation (verifies it doesn't crash/panic with division-by-zero)
	err := reconciler.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile() failed with unexpected error: %v", err)
	}

	snapshot, err := store.Get(ctx)
	if err != nil {
		t.Fatalf("failed to get snapshot: %v", err)
	}
	// 4. Assert Project-level math fallbacks:
	// MaxRPM is 0. UsableRPM = 0. Usage = 100 (from mock). Headroom = 0 - 100 = -100.
	p, err := snapshot.GetProjectQuota("projecta", "us-central1", "model1_pro")
	if err != nil {
		t.Fatalf("failed to find project quota: %v", err)
	}
	if p.HeadroomRpm != -100 {
		t.Errorf("Expected project RPM headroom to be -100, got %d", p.HeadroomRpm)
	}
	if p.UtilizationRpm != 0.0 {
		t.Errorf("Expected project RPM utilization to default to 0.0, got %f", p.UtilizationRpm)
	}

	// 5. Assert Org-level math fallbacks:
	org, err := snapshot.GetOrgQuota("us-central1", "model1_pro")
	if err != nil {
		t.Fatalf("failed to find org quota: %v", err)
	}
	if org.HeadroomRpm != -100 {
		t.Errorf("Expected org RPM headroom to be -100, got %d", org.HeadroomRpm)
	}
	if org.UtilizationRpm != 0.0 {
		t.Errorf("Expected org RPM utilization to default to 0.0, got %f", org.UtilizationRpm)
	}
}

func TestReconciler_ErrorResilience(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	cfg := &governor.Config{
		ProjectIDs:   []string{"projecta", "projectb"},
		Regions:      []string{"us-central1", "us-west1"},
		Models:       []string{"model1_flash", "model1_pro"},
		SafetyMargin: 0.3,
		DefaultProjectLimits: map[string]pb.ModelLimit{
			"projecta/us-central1/model1_pro": {MaxRpm: 100, MaxTpm: 5000},
		},
		DefaultOrgLimits: map[string]pb.ModelLimit{
			"us-central1/model1_pro": {MaxRpm: 500, MaxTpm: 25000},
		},
	}

	// 2. Get client and reconciler
	client := NewMockGCPClient(ctx, cfg.DefaultProjectLimits, cfg.DefaultOrgLimits)
	reconciler, err := NewReconciler(cfg, client, store)
	if err != nil {
		t.Fatalf("Failed to create reconciler: %v", err)
	}
	mockQuota := client.quotaClient.(*MockQuotaRequestClient)
	mockQuota.Err = errors.New("temporary gcp timeout")

	err = reconciler.reconcile(ctx)
	if err == nil {
		t.Error("Expected reconcile to fail during client outage, but it succeeded")
	}

	// Verify no snapshot was saved (store should return error because it's empty)
	_, err = store.Get(ctx)
	if err == nil {
		t.Error("Expected store to be empty during outage, but found a snapshot")
	}
}

func TestNewReconciler_Validation(t *testing.T) {
	ctx := context.Background()
	cfg := &governor.Config{}
	client := NewMockGCPClient(ctx, nil, nil)
	store := NewInMemoryStore()

	if _, err := NewReconciler(nil, client, store); err == nil {
		t.Error("Expected error for nil config, got nil")
	}
	if _, err := NewReconciler(cfg, nil, store); err == nil {
		t.Error("Expected error for nil client, got nil")
	}
	if _, err := NewReconciler(cfg, client, nil); err == nil {
		t.Error("Expected error for nil store, got nil")
	}
}

type mockBadMetricsClient struct {
	ReturnValues map[string]*RawModelQuota
}

func (m *mockBadMetricsClient) FetchMetrics(ctx context.Context, projectIDs []string, regions []string, models []string) (map[string]*RawModelQuota, error) {
	return m.ReturnValues, nil
}

func TestReconcile_KeyFailure(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	cfg := &governor.Config{
		ProjectIDs: []string{"projecta"},
		Regions:    []string{"us-central1"},
		Models:     []string{"model1"},
	}

	// This client returns a RawModelQuota with a ProjectId but empty Region.
	// This will cause raw.GetKey() to fail inside reconciler.reconcile()
	badClient := &mockBadMetricsClient{
		ReturnValues: map[string]*RawModelQuota{
			"bad-key": {
				ProjectId: "projecta",
				Region:    "", // Empty region triggers GetKey error
				Model:     "model1",
			},
		},
	}

	reconciler, err := NewReconciler(cfg, badClient, store)
	if err != nil {
		t.Fatalf("failed to create reconciler: %v", err)
	}

	err = reconciler.reconcile(ctx)
	if err == nil {
		t.Error("Expected reconcile to fail due to bad metric key, but it succeeded")
	}
}
