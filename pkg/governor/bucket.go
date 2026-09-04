package governor

import (
	"sync"
	"time"
)

// TokenBucket implements a thread-safe dual-rate limiter for RPM (Requests Per Minute)
// and TPM (Tokens Per Minute) with continuous timestamp-based token replenishment.
type TokenBucket struct {
	mu sync.Mutex

	maxRPM int64
	maxTPM int64

	currentRPM int64
	currentTPM int64

	// Adding seperate clocks because we want to advance clcoks only by the amount of token/request we gain and not elapsed time.
	// As RPM and TPM are different, they advance at different rates.
	// This implemnetation prevents the faster rate from stealing quota from the slower rate.
	lastRefillRPM time.Time
	lastRefillTPM time.Time
}

// NewTokenBucket creates a new dual-counter TokenBucket initialized to full capacity.
func NewTokenBucket(maxRPM, maxTPM int64) *TokenBucket {
	now := time.Now()
	return &TokenBucket{
		maxRPM:        maxRPM,
		maxTPM:        maxTPM,
		currentRPM:    maxRPM,
		currentTPM:    maxTPM,
		lastRefillRPM: now,
		lastRefillTPM: now,
	}
}

// SetCapacity dynamically recalibrates the bucket capacities (e.g. on snapshot sync).
func (tb *TokenBucket) SetCapacity(maxRPM, maxTPM int64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refillLocked(time.Now())

	tb.maxRPM = maxRPM
	tb.maxTPM = maxTPM

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

	reqTokens := tokens
	if reqTokens < 0 {
		reqTokens = 0
	}

	// Check if sufficient tokens and request capacity exist
	if tb.currentRPM >= 1 && tb.currentTPM >= reqTokens {
		tb.currentRPM -= 1
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

	tb.currentTPM = min(tb.maxTPM, tb.currentTPM+tokens)
}

// GetAvailable returns the currently available RPM and TPM tokens.
func (tb *TokenBucket) GetAvailable() (availRPM, availTPM int64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refillLocked(time.Now())
	return tb.currentRPM, tb.currentTPM
}

// refillLocked recalculates token levels based on elapsed time since last refill.
// Caller must hold tb.mu.
func (tb *TokenBucket) refillLocked(now time.Time) {
	elapsedTPM := now.Sub(tb.lastRefillTPM)
	if elapsedTPM > 0 && tb.maxTPM > 0 {
		nanosPerToken := int64(60*time.Second) / tb.maxTPM // how many nanoseconds to get 1 token
		if nanosPerToken > 0 {
			newTokens := elapsedTPM.Nanoseconds() / nanosPerToken
			if newTokens > 0 {
				tb.currentTPM = min(tb.maxTPM, tb.currentTPM+newTokens)
				tb.lastRefillTPM = tb.lastRefillTPM.Add(time.Duration(newTokens * nanosPerToken)) // to ensure you are not losing any time, for example when number of tokens attained is rounded down.
				// Eg: elapsed time is 25ms, tokens = 25/10 = 2 tokens, these 2 tokens only cost 20ms (not 25). if lastRefill is 25, we lose 5ms of progress due to rounding down.
			}
		}
	}

	elapsedRPM := now.Sub(tb.lastRefillRPM)
	if elapsedRPM > 0 && tb.maxRPM > 0 {
		nanosPerRequest := int64(60*time.Second) / tb.maxRPM
		if nanosPerRequest > 0 {
			newRequests := elapsedRPM.Nanoseconds() / nanosPerRequest
			if newRequests > 0 {
				tb.currentRPM = min(tb.maxRPM, tb.currentRPM+newRequests)
				tb.lastRefillRPM = tb.lastRefillRPM.Add(time.Duration(newRequests * nanosPerRequest))
			}
		}
	}
}

func (tb *TokenBucket) SyncHeadroom(headroomRPM, headroomTPM int64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()

	tb.refillLocked(now)
	if headroomRPM <= 0 {
		tb.currentRPM = 0
	} else {
		tb.currentRPM = min(tb.currentRPM, headroomRPM)
	}
	if headroomTPM <= 0 {
		tb.currentTPM = 0
	} else {
		tb.currentTPM = min(tb.currentTPM, headroomTPM)
	}
	// Reset the clock to now so we refill from this new snapshot baseline
	tb.lastRefillRPM = now
	tb.lastRefillTPM = now
}
