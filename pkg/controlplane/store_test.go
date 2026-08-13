package controlplane

import (
	"context"
	"sync"
	"testing"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestInMemoryStore_SaveAndGet(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	// 1. Construct a dummy pb.QuotaSnapshot
	snapshot := &pb.QuotaSnapshot{
		OrgQuotas: map[string]*pb.ModelQuota{
			"us-central1/gemini-2.5-pro": {
				Model:  "gemini-2.5-pro",
				Region: "us-central1",
				MaxRpm: 100,
			},
		},
		ProjectQuotas: map[string]*pb.ModelQuota{
			"project-a/us-central1/gemini-2.5-pro": {
				ProjectId: "project-a",
				Model:     "gemini-2.5-pro",
				Region:    "us-central1",
				MaxRpm:    100,
			},
		},
		LastSyncedAt: timestamppb.Now(),
	}

	err := store.Save(ctx, snapshot)
	if err != nil {
		t.Fatalf("Save() failed unexpectedly: %v", err)
	}
	retrieved, err := store.Get(ctx)
	if err != nil {
		t.Fatalf("Get() failed unexpectedly: %v", err)
	}

	if retrieved != snapshot {
		t.Errorf("Get() returned a different snapshot pointer. Got %p, want %p", retrieved, snapshot)
	}
}

func TestInMemoryStore_SaveNil(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	err := store.Save(ctx, nil)

	if err == nil {
		t.Errorf("Expected error, but got none")
	}
}

func TestInMemoryStore_GetEmpty(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	qs, err := store.Get(ctx)

	if qs != nil {
		t.Fatalf("Expected nil value")
	}

	if err == nil {
		t.Errorf("Expected error, but got none")
	}
}

func TestInMemoryStore_Concurrency(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers * 2)

	// Start Writers
	for i := 1; i <= workers; i++ {
		go func(val int64) {
			defer wg.Done()
			snap := &pb.QuotaSnapshot{
				OrgQuotas: map[string]*pb.ModelQuota{
					"us-central1/gemini-3.5-pro": {
						MaxRpm: val,
					},
				},
				ProjectQuotas: map[string]*pb.ModelQuota{
					"project-a/us-central1/gemini-3.5-pro": {
						ProjectId: "project-a",
						MaxRpm:    val,
					},
				},
			}
			_ = store.Save(ctx, snap)
		}(int64(i))
	}

	// Start Readers
	for i := 1; i <= workers; i++ {
		go func() {
			defer wg.Done()
			_, _ = store.Get(ctx)
		}()
	}

	wg.Wait()
}
