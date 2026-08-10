package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

// GetTestReconciler helper accepts the config and store from the test
func GetTestReconciler(cfg *governor.Config, store SnapshotStore) *Reconciler {
	quotaClient := NewMockQuotaClient(cfg.DefaultProjectLimits, cfg.DefaultOrgLimits)
	reconciler, err := NewReconciler(cfg, quotaClient, store)
	if err != nil {
		panic(err)
	}
	return reconciler
}

func TestReconcile_AggregatesAndCalculates(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	cfg := &governor.Config{
		ProjectIDs:   []string{"projecta", "projectb"},
		Regions:      []string{"us-central1", "us-west1"},
		Models:       []string{"model1_flash", "model1_pro"},
		SafetyMargin: 0.3,
		DefaultProjectLimits: map[string]governor.ModelLimit{
			"projecta/us-central1/model1_pro":   {MaxRPM: 100, MaxTPM: 5000},
			"projecta/us-west1/model1_pro":      {MaxRPM: 100, MaxTPM: 5000},
			"projecta/us-central1/model1_flash": {MaxRPM: 50, MaxTPM: 2500},
			"projecta/us-west1/model1_flash":    {MaxRPM: 50, MaxTPM: 2500},
			"projectb/us-central1/model1_flash": {MaxRPM: 50, MaxTPM: 2500},
			"projectb/us-west1/model1_flash":    {MaxRPM: 50, MaxTPM: 2500},
		},
		DefaultOrgLimits: map[string]governor.ModelLimit{
			"us-central1/model1_pro":   {MaxRPM: 500, MaxTPM: 25000},
			"us-west1/model1_pro":      {MaxRPM: 500, MaxTPM: 25000},
			"us-central1/model1_flash": {MaxRPM: 200, MaxTPM: 10000},
			"us-west1/model1_flash":    {MaxRPM: 200, MaxTPM: 10000},
		},
	}

	reconciler := GetTestReconciler(cfg, store)

	err := reconciler.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile() failed: %v", err)
	}

	snapshot, err := store.Get(ctx)
	if err != nil {
		t.Fatalf("failed to get snapshot: %v", err)
	}

	p1, err := snapshot.GetProjectQuota("projecta", "us-central1", "model1_pro")
	if err != nil {
		t.Errorf("Failed to find project quota: %v", err)
	} else {
		if p1.MaxRPM != 100 || p1.MaxTPM != 5000 {
			t.Errorf("Expected project limit 100 RPM / 5000 TPM, got %d RPM / %d TPM", p1.MaxRPM, p1.MaxTPM)
		}
		if p1.HeadroomRPM != -30 {
			t.Errorf("Expected project RPM headroom -30, got %d", p1.HeadroomRPM)
		}
	}
}

type SpyStore struct {
	InMemoryStore
	saveCount int
	mu        sync.Mutex
}

func (s *SpyStore) Save(ctx context.Context, snapshot *governor.QuotaSnapshot) error {
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
		DefaultProjectLimits: map[string]governor.ModelLimit{
			"projecta/us-central1/model1_pro": {MaxRPM: 100, MaxTPM: 5000},
		},
		DefaultOrgLimits: map[string]governor.ModelLimit{
			"us-central1/model1_pro": {MaxRPM: 500, MaxTPM: 25000},
		},
	}

	reconciler := GetTestReconciler(cfg, spyStore)
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
		DefaultProjectLimits: map[string]governor.ModelLimit{
			// Max limit is 0
			"projecta/us-central1/model1_pro": {MaxRPM: 0, MaxTPM: 0},
		},
		DefaultOrgLimits: map[string]governor.ModelLimit{
			// Org limit is also 0
			"us-central1/model1_pro": {MaxRPM: 0, MaxTPM: 0},
		},
	}

	// 2. Setup Reconciler
	reconciler := GetTestReconciler(cfg, store)

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
	if p.HeadroomRPM != -100 {
		t.Errorf("Expected project RPM headroom to be -100, got %d", p.HeadroomRPM)
	}
	if p.UtilizationRPM != 0.0 {
		t.Errorf("Expected project RPM utilization to default to 0.0, got %f", p.UtilizationRPM)
	}

	// 5. Assert Org-level math fallbacks:
	org, err := snapshot.GetOrgQuota("us-central1", "model1_pro")
	if err != nil {
		t.Fatalf("failed to find org quota: %v", err)
	}
	if org.HeadroomRPM != -100 {
		t.Errorf("Expected org RPM headroom to be -100, got %d", org.HeadroomRPM)
	}
	if org.UtilizationRPM != 0.0 {
		t.Errorf("Expected org RPM utilization to default to 0.0, got %f", org.UtilizationRPM)
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
		DefaultProjectLimits: map[string]governor.ModelLimit{
			"projecta/us-central1/model1_pro": {MaxRPM: 100, MaxTPM: 5000},
		},
		DefaultOrgLimits: map[string]governor.ModelLimit{
			"us-central1/model1_pro": {MaxRPM: 500, MaxTPM: 25000},
		},
	}

	// 2. Get client and reconciler
	client := NewMockQuotaClient(cfg.DefaultProjectLimits, cfg.DefaultOrgLimits)
	reconciler, err := NewReconciler(cfg, client, store)
	if err != nil {
		t.Fatalf("Failed to create reconciler: %v", err)
	}
	client.Err = errors.New("temporary gcp timeout")

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
