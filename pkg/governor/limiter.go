package governor

import (
	"fmt"
	"strings"
	"time"
)

// unlimitedCapacity stands in for "no limit" on one dimension of an allowance
// (for example a TPM-only allowance has no RPM limit). It is large enough to
// never run out, and small enough that floor maths cannot overflow int64.
const unlimitedCapacity int64 = 1 << 40

// Limiter enforces the configured allowances locally in the data plane, so
// admission does not depend on a 30-second-old snapshot. It holds one token
// bucket per "location/model" allowance, sized to this replica's share.
//
// The set of buckets is fixed at construction, so lookups need no lock; each
// bucket has its own mutex.
type Limiter struct {
	buckets        map[string]*TokenBucket
	reservePercent int64
}

// NewLimiter builds a Limiter from a loaded Config. Each allowance is split
// evenly across cfg.Replicas() data plane replicas. clock may be nil
// (time.Now).
func NewLimiter(cfg *Config, clock func() time.Time) (*Limiter, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	replicas := int64(cfg.Replicas())
	l := &Limiter{
		buckets:        make(map[string]*TokenBucket, len(cfg.Allowances)),
		reservePercent: cfg.ReservePercent(),
	}
	for key, a := range cfg.Allowances {
		if a == nil {
			continue
		}
		l.buckets[strings.ToLower(key)] = NewTokenBucketWithClock(share(a.MaxRpm, replicas), share(a.MaxTpm, replicas), clock)
	}
	return l, nil
}

// share returns one replica's slice of a limit. A limit of 0 means "no limit".
func share(limit, replicas int64) int64 {
	if limit <= 0 {
		return unlimitedCapacity
	}
	return max(limit/max(replicas, 1), 1)
}

func (l *Limiter) bucket(location, model string) *TokenBucket {
	if l == nil {
		return nil
	}
	return l.buckets[strings.ToLower(location)+"/"+strings.ToLower(model)]
}

// Known reports whether an allowance is configured for location/model.
func (l *Limiter) Known(location, model string) bool {
	return l.bucket(location, model) != nil
}

// TryAdmit reserves one request and tokens for best-effort or custom traffic,
// without dipping into the critical reserve. known is false when no allowance
// is configured for the target, in which case nothing is reserved.
func (l *Limiter) TryAdmit(location, model string, tokens int64) (ok, known bool) {
	b := l.bucket(location, model)
	if b == nil {
		return false, false
	}
	return b.TryAcquireAbove(tokens, l.reservePercent), true
}

// AdmitCritical always reserves one request and tokens for critical traffic,
// including from the reserve and beyond it (as debt). within reports whether
// the request fitted inside this replica's allowance.
func (l *Limiter) AdmitCritical(location, model string, tokens int64) (within, known bool) {
	b := l.bucket(location, model)
	if b == nil {
		return true, false
	}
	return b.ForceAcquire(tokens), true
}
