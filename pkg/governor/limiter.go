package governor

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
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

	// allowances holds the full org allowance per key; buckets hold this
	// replica's share of it.
	allowances map[string]ModelLimit
	// replicas is the fallback even split used until (and whenever) the
	// control plane does not provide demand-weighted shares.
	replicas int64
	// demand counts requests and tokens per key since the last DrainDemand,
	// admitted or not, so the control plane can size this replica's share.
	demand map[string]*demandCounter
}

type demandCounter struct {
	requests         atomic.Int64
	tokens           atomic.Int64
	admittedRequests atomic.Int64
	admittedTokens   atomic.Int64
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
		allowances:     make(map[string]ModelLimit, len(cfg.Allowances)),
		replicas:       replicas,
		demand:         make(map[string]*demandCounter, len(cfg.Allowances)),
	}
	for key, a := range cfg.Allowances {
		if a == nil {
			continue
		}
		k := strings.ToLower(key)
		l.allowances[k] = *a
		l.demand[k] = &demandCounter{}
		l.buckets[k] = NewTokenBucketWithClock(share(a.MaxRpm, replicas), share(a.MaxTpm, replicas), clock)
	}
	return l, nil
}

// fractionOf returns a fraction (0-1] of a limit. A limit of 0 means "no limit".
func fractionOf(limit int64, f float64) int64 {
	if limit <= 0 {
		return unlimitedCapacity
	}
	if f <= 0 || f > 1 {
		f = 1
	}
	return max(int64(float64(limit)*f), 1)
}

// ApplyShares resizes this replica's buckets to the demand-weighted shares
// computed by the control plane. Unknown targets are ignored, and targets
// missing from shares keep their current size.
func (l *Limiter) ApplyShares(shares []*pb.TargetShare) {
	if l == nil {
		return
	}
	for _, sh := range shares {
		k := strings.ToLower(strings.TrimSpace(sh.GetTarget()))
		b, ok := l.buckets[k]
		if !ok {
			continue
		}
		a := l.allowances[k]
		b.SetCapacity(fractionOf(a.MaxRpm, sh.GetRpmFraction()), fractionOf(a.MaxTpm, sh.GetTpmFraction()))
	}
}

// ResetShares returns every bucket to the even split across the configured
// dataplane_replicas. Used when the control plane is unreachable.
func (l *Limiter) ResetShares() {
	if l == nil {
		return
	}
	for k, b := range l.buckets {
		a := l.allowances[k]
		b.SetCapacity(share(a.MaxRpm, l.replicas), share(a.MaxTpm, l.replicas))
	}
}

// DrainDemand returns the demand seen per target since the previous call and
// resets the counters. Targets with no demand are omitted.
func (l *Limiter) DrainDemand() []*pb.TargetDemand {
	if l == nil {
		return nil
	}
	out := make([]*pb.TargetDemand, 0, len(l.demand))
	for k, d := range l.demand {
		req, tok := d.requests.Swap(0), d.tokens.Swap(0)
		areq, atok := d.admittedRequests.Swap(0), d.admittedTokens.Swap(0)
		if req == 0 && tok == 0 && areq == 0 && atok == 0 {
			continue
		}
		out = append(out, &pb.TargetDemand{Target: k, Requests: req, Tokens: tok, AdmittedRequests: areq, AdmittedTokens: atok})
	}
	return out
}

// Available is the current balance of one bucket.
type Available struct {
	Requests int64
	Tokens   int64
}

// Available returns the current balance of every bucket, keyed by
// "location/model". Unlimited dimensions are reported as 0 so dashboards are
// not dominated by the internal "no limit" sentinel.
func (l *Limiter) Available() map[string]Available {
	out := map[string]Available{}
	if l == nil {
		return out
	}
	for k, b := range l.buckets {
		rpm, tpm := b.GetAvailable()
		a := l.allowances[k]
		if a.MaxRpm <= 0 {
			rpm = 0
		}
		if a.MaxTpm <= 0 {
			tpm = 0
		}
		out[k] = Available{Requests: rpm, Tokens: tpm}
	}
	return out
}

func (l *Limiter) record(location, model string, tokens int64, admitted bool) {
	if d := l.demand[strings.ToLower(location)+"/"+strings.ToLower(model)]; d != nil {
		t := max(tokens, 0)
		d.requests.Add(1)
		d.tokens.Add(t)
		if admitted {
			d.admittedRequests.Add(1)
			d.admittedTokens.Add(t)
		}
	}
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
	ok = b.TryAcquireAbove(tokens, l.reservePercent)
	l.record(location, model, tokens, ok)
	return ok, true
}

// AdmitCritical always reserves one request and tokens for critical traffic,
// including from the reserve and beyond it (as debt). within reports whether
// the request fitted inside this replica's allowance.
func (l *Limiter) AdmitCritical(location, model string, tokens int64) (within, known bool) {
	b := l.bucket(location, model)
	if b == nil {
		return true, false
	}
	l.record(location, model, tokens, true)
	return b.ForceAcquire(tokens), true
}
