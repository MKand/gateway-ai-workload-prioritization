package controlplane

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

const (
	// bypassWindowMinutes is how many recent complete minutes are compared;
	// the largest bypass among them is used, so a spike keeps shrinking the
	// budget until it has clearly ended.
	bypassWindowMinutes = 3

	// bypassStaleAfter is how long a Monitoring reading stays usable. After
	// that the tracker stops trusting its estimate and falls back to
	// "allowance minus margin".
	bypassStaleAfter = 5 * time.Minute

	// admittedRetention is how many minutes of governor-admitted counts are
	// kept. Monitoring lags by a few minutes, so this must cover that lag.
	admittedRetention = 15

	// minAllowanceFactor stops a huge bypass estimate from shrinking an
	// allowance to nothing. Critical traffic is never shed anyway.
	minAllowanceFactor = 0.01
)

// MinuteUsage is requests and input tokens in one wall-clock minute.
type MinuteUsage struct {
	Requests int64
	Tokens   int64
}

// UsageSeries is org-wide usage per "location/model" target, keyed by the
// Unix time of each minute's start.
type UsageSeries map[string]map[int64]MinuteUsage

// BypassTracker estimates traffic that reaches Vertex AI without going
// through the governor, and turns it into a factor that shrinks each
// allowance:
//
//	bypass(M) = max(0, org_usage(M) - governor_admitted(M))
//	factor    = (allowance - max(bypass over last 3 minutes) - margin * allowance) / allowance
//
// Org usage comes from Cloud Monitoring, which lags by a few minutes, so
// each comparison uses the same minute on both sides. It is safe for
// concurrent use.
type BypassTracker struct {
	mu         sync.Mutex
	now        func() time.Time
	allowances map[string]governor.ModelLimit
	margin     float64

	admitted map[string]map[int64]MinuteUsage // target -> minute -> admitted
	usage    UsageSeries
	updated  time.Time // last successful Monitoring read; zero = never
}

// NewBypassTracker builds a tracker for the configured allowances. clock may
// be nil (time.Now).
func NewBypassTracker(cfg *governor.Config, clock func() time.Time) *BypassTracker {
	if clock == nil {
		clock = time.Now
	}
	t := &BypassTracker{
		now:        clock,
		allowances: map[string]governor.ModelLimit{},
		margin:     float64(cfg.BypassMargin()) / 100,
		admitted:   map[string]map[int64]MinuteUsage{},
		usage:      UsageSeries{},
	}
	for k, a := range cfg.Allowances {
		if a != nil {
			t.allowances[strings.ToLower(k)] = *a
		}
	}
	return t
}

// Targets returns every configured allowance target, sorted.
func (t *BypassTracker) Targets() []string {
	out := make([]string, 0, len(t.allowances))
	for k := range t.allowances {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RecordAdmitted adds requests and tokens the governor admitted for target,
// attributed to the minute containing at.
func (t *BypassTracker) RecordAdmitted(target string, at time.Time, requests, tokens int64) {
	target = strings.ToLower(strings.TrimSpace(target))
	if _, ok := t.allowances[target]; !ok || (requests <= 0 && tokens <= 0) {
		return
	}
	minute := at.Truncate(time.Minute).Unix()
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.admitted[target]
	if m == nil {
		m = map[int64]MinuteUsage{}
		t.admitted[target] = m
	}
	u := m[minute]
	u.Requests += max(requests, 0)
	u.Tokens += max(tokens, 0)
	m[minute] = u
	cutoff := minute - admittedRetention*60
	for k := range m {
		if k < cutoff {
			delete(m, k)
		}
	}
}

// Update stores a fresh Monitoring reading of org-wide usage.
func (t *BypassTracker) Update(series UsageSeries) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.usage = series
	t.updated = t.now()
}

// Estimate is the bypass estimate and resulting allowance factors for one
// target.
type Estimate struct {
	BypassRequests int64
	BypassTokens   int64
	RPMFactor      float64
	TPMFactor      float64
	// Stale is true when Monitoring data is older than 5 minutes, so the
	// factors fall back to 1 - margin.
	Stale bool
}

// Estimate returns the current bypass estimate for target. Before the first
// Monitoring reading the factors are 1 (nothing is known, nothing is
// subtracted).
func (t *BypassTracker) Estimate(target string) Estimate {
	target = strings.ToLower(target)
	t.mu.Lock()
	defer t.mu.Unlock()

	a, ok := t.allowances[target]
	if !ok || t.updated.IsZero() {
		return Estimate{RPMFactor: 1, TPMFactor: 1}
	}
	if t.now().Sub(t.updated) > bypassStaleAfter {
		f := clampFactor(1 - t.margin)
		return Estimate{RPMFactor: f, TPMFactor: f, Stale: true}
	}

	// Only complete minutes count: the current minute is still filling up.
	current := t.now().Truncate(time.Minute).Unix()
	usage := t.usage[target]
	minutes := make([]int64, 0, len(usage))
	for m := range usage {
		if m < current {
			minutes = append(minutes, m)
		}
	}
	sort.Slice(minutes, func(i, j int) bool { return minutes[i] > minutes[j] })
	if len(minutes) > bypassWindowMinutes {
		minutes = minutes[:bypassWindowMinutes]
	}

	var e Estimate
	for _, m := range minutes {
		got := t.admitted[target][m]
		e.BypassRequests = max(e.BypassRequests, usage[m].Requests-got.Requests)
		e.BypassTokens = max(e.BypassTokens, usage[m].Tokens-got.Tokens)
	}
	e.RPMFactor = factor(a.MaxRpm, e.BypassRequests, t.margin)
	e.TPMFactor = factor(a.MaxTpm, e.BypassTokens, t.margin)
	return e
}

// factor is (limit - bypass - margin*limit) / limit, clamped. A limit of 0
// means "no limit", so nothing is subtracted.
func factor(limit, bypass int64, margin float64) float64 {
	if limit <= 0 {
		return 1
	}
	return clampFactor(1 - margin - float64(max(bypass, 0))/float64(limit))
}

func clampFactor(f float64) float64 {
	return min(max(f, minAllowanceFactor), 1)
}

// DataAge returns how old the last Monitoring reading is, or -1 if there has
// been none.
func (t *BypassTracker) DataAge() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.updated.IsZero() {
		return -1
	}
	return t.now().Sub(t.updated)
}
