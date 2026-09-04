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
	// 1200 RPM (20/s = 1 req every 50ms), 120,000 TPM (2000/s)
	tb := governor.NewTokenBucket(1200, 120000)

	// Consume all RPM
	for i := 0; i < 1200; i++ {
		if !tb.TryAcquire(10) {
			t.Fatalf("failed to acquire on iteration %d", i)
		}
	}

	// Should be exhausted
	if tb.TryAcquire(10) {
		t.Errorf("expected bucket to be exhausted")
	}

	// Wait 60ms (replenishes at least 1 RPM and 120 TPM)
	time.Sleep(60 * time.Millisecond)

	if !tb.TryAcquire(10) {
		t.Errorf("expected bucket to replenish and succeed after 60ms")
	}
}

func TestTokenBucketRemainderPreservation(t *testing.T) {
	// 6000 TPM -> 1 token every 10ms (10,000,000 ns)
	// 60 RPM -> 1 req every 1000ms
	tb := governor.NewTokenBucket(60, 6000)

	// Drain all TPM
	if !tb.TryAcquire(6000) {
		t.Fatalf("failed to drain TPM")
	}

	_, availTPM := tb.GetAvailable()
	if availTPM != 0 {
		t.Fatalf("expected 0 TPM, got %d", availTPM)
	}

	// Step 1: Wait 25ms.
	// 25ms / 10ms = 2 tokens (costs 20ms). 5ms leftover unspent.
	time.Sleep(25 * time.Millisecond)
	_, availTPM = tb.GetAvailable()
	if availTPM < 2 {
		t.Fatalf("expected at least 2 tokens after 25ms, got %d", availTPM)
	}

	// Step 2: Wait another 6ms (total >30ms since drain).
	// With remainder preservation, 5ms leftover + 6ms new >= 10ms -> 3rd token earned!
	time.Sleep(6 * time.Millisecond)
	_, availTPM = tb.GetAvailable()
	if availTPM < 3 {
		t.Errorf("expected at least 3 tokens after remainder rollover, got %d", availTPM)
	}
}

func TestTokenBucketSyncHeadroom(t *testing.T) {
	t.Run("clamps local capacity down when GCP headroom is lower", func(t *testing.T) {
		tb := governor.NewTokenBucket(100, 10000)

		// GCP reports only 30 RPM and 3000 TPM remaining
		tb.SyncHeadroom(30, 3000)

		availRPM, availTPM := tb.GetAvailable()
		if availRPM != 30 || availTPM != 3000 {
			t.Errorf("expected clamped headroom (30, 3000), got (%d, %d)", availRPM, availTPM)
		}
	})

	t.Run("retains lower local count when local traffic was higher than GCP telemetry lag", func(t *testing.T) {
		tb := governor.NewTokenBucket(100, 10000)

		// Drain locally down to 10 RPM and 1000 TPM
		for i := 0; i < 90; i++ {
			if !tb.TryAcquire(100) {
				t.Fatalf("failed acquire at %d", i)
			}
		}

		availRPM, availTPM := tb.GetAvailable()
		if availRPM != 10 || availTPM != 1000 {
			t.Fatalf("expected local (10, 1000), got (%d, %d)", availRPM, availTPM)
		}

		// GCP snapshot arrives with delayed telemetry reporting 50 RPM and 5000 TPM
		// Must NOT resurrect spent tokens!
		tb.SyncHeadroom(50, 5000)

		availRPM, availTPM = tb.GetAvailable()
		if availRPM != 10 || availTPM != 1000 {
			t.Errorf("pessimistic clamp must retain lower local count (10, 1000), got (%d, %d)", availRPM, availTPM)
		}
	})

	t.Run("zero or negative headroom clamps to zero", func(t *testing.T) {
		tb := governor.NewTokenBucket(100, 10000)

		tb.SyncHeadroom(-5, 0)

		availRPM, availTPM := tb.GetAvailable()
		if availRPM != 0 || availTPM != 0 {
			t.Errorf("expected (0, 0) for negative headroom, got (%d, %d)", availRPM, availTPM)
		}
	})
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

func TestTokenBucketZeroLimits(t *testing.T) {
	// Zero limits should never panic or divide by zero
	tb := governor.NewTokenBucket(0, 0)

	availRPM, availTPM := tb.GetAvailable()
	if availRPM != 0 || availTPM != 0 {
		t.Errorf("expected (0, 0), got (%d, %d)", availRPM, availTPM)
	}

	if tb.TryAcquire(10) {
		t.Errorf("acquire must fail when limits are 0")
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
				if j%10 == 0 {
					tb.SyncHeadroom(800, 400000)
				}
				if j%20 == 0 {
					tb.GetAvailable()
				}
			}
		}(i)
	}

	wg.Wait()
}
