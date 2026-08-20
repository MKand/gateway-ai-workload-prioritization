package governor_test

import (
	"sync"
	"testing"
	"time"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

func TestTokenBucketBasic(t *testing.T) {
	// 60 RPM = 1 req/sec, 60,000 TPM = 1,000 tokens/sec
	tb := governor.NewTokenBucket(60, 60000)

	availRPM, availTPM := tb.GetAvailable()
	if availRPM != 60 || availTPM != 60000 {
		t.Fatalf("expected initial capacity (60, 60000), got (%d, %d)", availRPM, availTPM)
	}

	// 1. Successful acquisition
	if !tb.TryAcquire(1000) {
		t.Errorf("expected TryAcquire(1000) to succeed")
	}

	availRPM, availTPM = tb.GetAvailable()
	if availRPM != 59 || availTPM != 59000 {
		t.Errorf("expected (59, 59000), got (%d, %d)", availRPM, availTPM)
	}

	// 2. Over-capacity TPM acquisition should fail without deducting RPM
	if tb.TryAcquire(100000) {
		t.Errorf("expected TryAcquire(100000) to fail")
	}

	availRPM, availTPM = tb.GetAvailable()
	if availRPM != 59 {
		t.Errorf("failed acquire should not deduct RPM, got %d", availRPM)
	}
}

func TestTokenBucketRefill(t *testing.T) {
	// 60 RPM (1/s), 6000 TPM (100/s)
	tb := governor.NewTokenBucket(60, 6000)

	// Consume all RPM
	for i := 0; i < 60; i++ {
		if !tb.TryAcquire(10) {
			t.Fatalf("failed to acquire on iteration %d", i)
		}
	}

	// Should be exhausted
	if tb.TryAcquire(10) {
		t.Errorf("expected bucket to be exhausted")
	}

	// Wait for replenishment (150ms ~ 0.15s should replenish at least partial token, 1.1s replenishes at least 1 RPM)
	time.Sleep(1100 * time.Millisecond)

	if !tb.TryAcquire(10) {
		t.Errorf("expected bucket to replenish and succeed after 1.1s")
	}
}

func TestTokenBucketRefund(t *testing.T) {
	tb := governor.NewTokenBucket(10, 1000)

	// Reserve 500 tokens
	if !tb.TryAcquire(500) {
		t.Fatalf("failed initial acquire")
	}

	_, availTPM := tb.GetAvailable()
	if availTPM != 500 {
		t.Fatalf("expected 500 available TPM, got %d", availTPM)
	}

	// Actual response only used 200 tokens -> refund 300
	tb.Refund(300)

	_, availTPM = tb.GetAvailable()
	if availTPM != 800 {
		t.Errorf("expected 800 available TPM after refund, got %d", availTPM)
	}
}

func TestTokenBucketSetCapacity(t *testing.T) {
	tb := governor.NewTokenBucket(100, 10000)

	// Set lower capacity
	tb.SetCapacity(50, 5000)

	availRPM, availTPM := tb.GetAvailable()
	if availRPM > 50 || availTPM > 5000 {
		t.Errorf("expected clamped available tokens <= (50, 5000), got (%d, %d)", availRPM, availTPM)
	}
}

func TestTokenBucketConcurrentRace(t *testing.T) {
	tb := governor.NewTokenBucket(1000, 500000)

	var wg sync.WaitGroup
	workers := 50
	iterations := 100

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				tb.TryAcquire(10)
				if j%5 == 0 {
					tb.Refund(5)
				}
				if j%20 == 0 {
					tb.GetAvailable()
				}
			}
		}(i)
	}

	wg.Wait()
}
