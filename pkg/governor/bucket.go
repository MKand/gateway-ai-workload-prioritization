package governor

import (
	"math"
	"sync"
	"time"
)

// TokenBucket implements a thread-safe dual-rate limiter for RPM (Requests Per Minute)
// and TPM (Tokens Per Minute) with continuous timestamp-based token replenishment.
type TokenBucket struct {
	mu sync.Mutex

	maxRPM float64
	maxTPM float64

	currentRPM float64
	currentTPM float64

	lastRefill time.Time
}

// NewTokenBucket creates a new dual-counter TokenBucket initialized to full capacity.
func NewTokenBucket(maxRPM, maxTPM int64) *TokenBucket {
	now := time.Now()
	return &TokenBucket{
		maxRPM:     float64(maxRPM),
		maxTPM:     float64(maxTPM),
		currentRPM: float64(maxRPM),
		currentTPM: float64(maxTPM),
		lastRefill: now,
	}
}

// SetCapacity dynamically recalibrates the bucket capacities (e.g. on snapshot sync).
func (tb *TokenBucket) SetCapacity(maxRPM, maxTPM int64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refillLocked(time.Now())

	tb.maxRPM = float64(maxRPM)
	tb.maxTPM = float64(maxTPM)

	// Clamp available tokens to new capacities
	if tb.currentRPM > tb.maxRPM {
		tb.currentRPM = tb.maxRPM
	}
	if tb.currentTPM > tb.maxTPM {
		tb.currentTPM = tb.maxTPM
	}
}

// TryAcquire attempts to reserve 1 request (RPM) and the specified estimated tokens (TPM).
// Returns true if both RPM and TPM are successfully reserved, false otherwise.
func (tb *TokenBucket) TryAcquire(tokens int64) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	tb.refillLocked(now)

	reqTokens := float64(tokens)
	if reqTokens < 0 {
		reqTokens = 0
	}

	// Check if sufficient tokens and request capacity exist
	if tb.currentRPM >= 1.0 && tb.currentTPM >= reqTokens {
		tb.currentRPM -= 1.0
		tb.currentTPM -= reqTokens
		return true
	}

	return false
}

// Refund adds back over-reserved tokens (e.g. when actual tokens < estimated reservation).
func (tb *TokenBucket) Refund(tokens int64) {
	if tokens <= 0 {
		return
	}

	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.currentTPM = math.Min(tb.maxTPM, tb.currentTPM+float64(tokens))
}

// GetAvailable returns the currently available RPM and TPM tokens.
func (tb *TokenBucket) GetAvailable() (availRPM, availTPM int64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refillLocked(time.Now())
	return int64(tb.currentRPM), int64(tb.currentTPM)
}

// refillLocked recalculates token levels based on elapsed time since last refill.
// Caller must hold tb.mu.
func (tb *TokenBucket) refillLocked(now time.Time) {
	elapsed := now.Sub(tb.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}

	// Replenish rate per second (capacity / 60 seconds)
	refillRPM := (tb.maxRPM / 60.0) * elapsed
	refillTPM := (tb.maxTPM / 60.0) * elapsed

	tb.currentRPM = math.Min(tb.maxRPM, tb.currentRPM+refillRPM)
	tb.currentTPM = math.Min(tb.maxTPM, tb.currentTPM+refillTPM)
	tb.lastRefill = now
}
