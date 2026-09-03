package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"

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

func TestInMemoryStore_Subscribe_basicdelivery(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	ch, u := store.Subscribe()
	defer u()

	snap := &pb.QuotaSnapshot{
		OrgQuotas: map[string]*pb.ModelQuota{
			"us-central1/gemini-3.5-pro": {
				MaxRpm: 100,
			},
		},
		ProjectQuotas: map[string]*pb.ModelQuota{
			"project-a/us-central1/gemini-3.5-pro": {
				ProjectId: "project-a",
				MaxRpm:    100,
			},
		},
	}

	if err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Save() failed unexpectedly: %v", err)
	}

	select {
	case received, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		if received != snap {
			t.Errorf("pointer mismatch: got %p, want %p", received, snap)
		}

	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for snapshot notification")
	}
}

func TestInMemoryStore_Subscribe_DropEvicted(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	ch, u := store.Subscribe()
	defer u()

	snap1 := &pb.QuotaSnapshot{
		OrgQuotas: map[string]*pb.ModelQuota{
			"us-central1/gemini-3.5-pro": {
				MaxRpm: 100,
			},
		},
		ProjectQuotas: map[string]*pb.ModelQuota{
			"project-a/us-central1/gemini-3.5-pro": {
				ProjectId: "project-a",
				MaxRpm:    100,
			},
		},
	}

	if err := store.Save(ctx, snap1); err != nil {
		t.Fatalf("Save() failed unexpectedly: %v", err)
	}

	snap2 := &pb.QuotaSnapshot{
		OrgQuotas: map[string]*pb.ModelQuota{
			"us-central1/gemini-3.5-pro": {
				MaxRpm: 110,
			},
		},
		ProjectQuotas: map[string]*pb.ModelQuota{
			"project-a/us-central1/gemini-3.5-pro": {
				ProjectId: "project-a",
				MaxRpm:    110,
			},
		},
	}
	if err := store.Save(ctx, snap2); err != nil {
		t.Fatalf("Save() failed unexpectedly: %v", err)
	}

	select {
	case received, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		if received != snap2 {
			t.Errorf("pointer mismatch: got %p, want %p", received, snap2)
		}

	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for snapshot notification")
	}

	// chan should be empty now
	select {
	case extra, _ := <-ch:
		t.Errorf("expected channel to be empty, but received: %v", extra)
	default:
		//this is good
	}
}

func TestInMemoryStore_Subscribe_Unsubscribe(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	ch, u := store.Subscribe()

	u()

	select {
	case _, ok := <-ch:

		if ok {
			t.Errorf("Expecting closed channel, but channel is open")
		}

	}

	err := store.Save(ctx, &pb.QuotaSnapshot{})
	if err != nil {
		t.Errorf("Channel not removed from subscribers")
	}

}

func TestInMemoryStore_Subscribe_SlowConsumerDoesNotBlockFast(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	fastCh, uFast := store.Subscribe()
	defer uFast()
	slowCh, uSlow := store.Subscribe()
	defer uSlow()

	// Fast consumer draining in the background
	fastReceivedCount := 0
	fastDone := make(chan struct{})

	go func() {
		defer close(fastDone)
		for {
			select {
			case _, ok := <-fastCh:
				if !ok {
					return
				}
				fastReceivedCount++
				if fastReceivedCount == 5 {
					return
				}
			case <-time.After(1 * time.Second):
				return
			}
		}
	}()

	// Save 5 snapshots rapidly; slowCh is never drained
	for i := 0; i < 5; i++ {
		snap := &pb.QuotaSnapshot{
			OrgQuotas: map[string]*pb.ModelQuota{
				"us-central1/gemini-3.5-pro": {MaxRpm: int64(100 + i)},
			},
		}
		if err := store.Save(ctx, snap); err != nil {
			t.Fatalf("Save(%d) failed: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 1. Verify fast consumer finished without waiting on slow consumer
	select {
	case <-fastDone:
		if fastReceivedCount != 5 {
			t.Errorf("expected fast consumer to receive 5 updates, got %d", fastReceivedCount)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("fast consumer timed out; likely blocked by slow consumer")
	}
	// 2. Verify slow consumer gets the latest snapshot (MaxRpm: 104)
	select {
	case latest, ok := <-slowCh:
		if !ok {
			t.Fatal("slowCh closed unexpectedly")
		}
		quota := latest.OrgQuotas["us-central1/gemini-3.5-pro"]
		if quota.MaxRpm != 104 {
			t.Errorf("expected slow consumer to have latest MaxRpm 104, got %d", quota.MaxRpm)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out reading from slow consumer")
	}
}
