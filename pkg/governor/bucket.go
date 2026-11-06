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

	// now is the bucket's clock. Tests inject a fake clock; production uses time.Now.
	now func() time.Time
}

// NewTokenBucket creates a new dual-counter TokenBucket initialized to full capacity.
func NewTokenBucket(maxRPM, maxTPM int64) *TokenBucket {
	return NewTokenBucketWithClock(maxRPM, maxTPM, time.Now)
}

// NewTokenBucketWithClock is NewTokenBucket with an injectable clock, so tests
// can control refill deterministically. A nil clock means time.Now.
func NewTokenBucketWithClock(maxRPM, maxTPM int64, clock func() time.Time) *TokenBucket {
	if clock == nil {
		clock = time.Now
	}
	now := clock()
	return &TokenBucket{
		now:           clock,
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

	tb.refillLocked(tb.now())

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

	now := tb.now()
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

// TryAcquireAbove is TryAcquire that refuses to dip into the bottom
// floorPercent of each capacity. It lets best-effort traffic use the bucket
// only down to the critical reserve. A dimension with capacity 0 is treated
// as having no floor.
func (tb *TokenBucket) TryAcquireAbove(tokens, floorPercent int64) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.refillLocked(tb.now())

	reqTokens := max(tokens, 0)
	floorRPM := tb.maxRPM * floorPercent / 100
	floorTPM := tb.maxTPM * floorPercent / 100
	if tb.currentRPM-1 >= floorRPM && tb.currentTPM-reqTokens >= floorTPM {
		tb.currentRPM--
		tb.currentTPM -= reqTokens
		return true
	}
	return false
}

// ForceAcquire always takes 1 request and the given tokens, letting the
// balance go negative (down to -capacity). It is used for critical traffic,
// which is never shed: the debt keeps best-effort traffic out until the
// overuse has been repaid by refill. It reports whether the request fitted
// within the available capacity.
func (tb *TokenBucket) ForceAcquire(tokens int64) (fitted bool) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.refillLocked(tb.now())

	reqTokens := max(tokens, 0)
	fitted = tb.currentRPM >= 1 && tb.currentTPM >= reqTokens
	tb.currentRPM = max(tb.currentRPM-1, -tb.maxRPM)
	tb.currentTPM = max(tb.currentTPM-reqTokens, -tb.maxTPM)
	return fitted
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

	tb.refillLocked(tb.now())
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
	now := tb.now()

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
