package governor_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

// fakeClock is a manually advanced clock for deterministic refill tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestTokenBucket_TryAcquireAboveStopsAtFloor(t *testing.T) {
	clk := newFakeClock()
	tb := governor.NewTokenBucketWithClock(100, 1_000_000, clk.Now)

	admitted := 0
	for tb.TryAcquireAbove(10, 30) {
		admitted++
	}
	if admitted != 70 {
		t.Fatalf("best effort admitted %d requests, want 70 (100 minus a 30%% reserve)", admitted)
	}
	if rpm, _ := tb.GetAvailable(); rpm != 30 {
		t.Fatalf("reserve left = %d, want 30", rpm)
	}

	// The reserve is still there for full acquisitions.
	for i := 0; i < 30; i++ {
		if !tb.TryAcquire(10) {
			t.Fatalf("critical acquire %d failed inside the reserve", i)
		}
	}

	// 100 RPM refills 1 request every 600ms; best effort needs the bucket above
	// the floor of 30 again, so it waits for 31 refilled requests.
	clk.Advance(30 * 600 * time.Millisecond)
	if tb.TryAcquireAbove(10, 30) {
		t.Fatal("best effort admitted at exactly the floor")
	}
	clk.Advance(600 * time.Millisecond)
	if !tb.TryAcquireAbove(10, 30) {
		t.Fatal("best effort refused one request above the floor")
	}
}

func TestTokenBucket_TryAcquireAboveTPMFloor(t *testing.T) {
	tb := governor.NewTokenBucketWithClock(1000, 10_000, newFakeClock().Now)
	if !tb.TryAcquireAbove(7_000, 30) {
		t.Fatal("7000 of 10000 tokens should fit above a 30% floor")
	}
	if tb.TryAcquireAbove(1, 30) {
		t.Fatal("TPM floor reached, acquire should fail even though RPM has room")
	}
}

func TestTokenBucket_ForceAcquireDebt(t *testing.T) {
	clk := newFakeClock()
	tb := governor.NewTokenBucketWithClock(10, 1000, clk.Now)

	for i := 0; i < 10; i++ {
		if !tb.ForceAcquire(0) {
			t.Fatalf("request %d should fit within capacity", i)
		}
	}
	if tb.ForceAcquire(0) {
		t.Fatal("11th request should report it did not fit")
	}
	if rpm, _ := tb.GetAvailable(); rpm != -1 {
		t.Fatalf("debt = %d, want -1", rpm)
	}

	// Debt is capped at -capacity.
	for i := 0; i < 50; i++ {
		tb.ForceAcquire(0)
	}
	if rpm, _ := tb.GetAvailable(); rpm != -10 {
		t.Fatalf("debt = %d, want capped at -10", rpm)
	}

	// Best effort stays out until the debt is repaid and the floor is passed:
	// 10 RPM refills 1 request every 6s; from -10 it needs 14 to reach 4 (> floor 3).
	clk.Advance(13 * 6 * time.Second)
	if tb.TryAcquireAbove(0, 30) {
		t.Fatal("best effort admitted before the debt was repaid")
	}
	clk.Advance(6 * time.Second)
	if !tb.TryAcquireAbove(0, 30) {
		t.Fatal("best effort refused after the debt was repaid")
	}
}

func limiterConfig(rpm, tpm int64, replicas int) *governor.Config {
	return &governor.Config{
		Allowances: map[string]*governor.ModelLimit{
			"us-central1/m":   {MaxRpm: rpm, MaxTpm: tpm},
			"europe-west1/m":  {MaxRpm: rpm, MaxTpm: tpm},
			"global/tpm-only": {MaxTpm: 10_000},
		},
		DataplaneReplicas: &replicas,
	}
}

func TestLimiter_SplitsAcrossReplicas(t *testing.T) {
	l, err := governor.NewLimiter(limiterConfig(100, 0, 2), newFakeClock().Now)
	if err != nil {
		t.Fatal(err)
	}
	admitted := 0
	for {
		ok, known := l.TryAdmit("US-Central1", "M", 0) // keys are case-insensitive
		if !known {
			t.Fatal("allowance should be known")
		}
		if !ok {
			break
		}
		admitted++
	}
	if admitted != 35 {
		t.Fatalf("admitted %d, want 35 (half of 100, minus a 30%% reserve)", admitted)
	}
}

func TestLimiter_UnlimitedDimensionAndUnknownTarget(t *testing.T) {
	l, err := governor.NewLimiter(limiterConfig(100, 0, 1), newFakeClock().Now)
	if err != nil {
		t.Fatal(err)
	}
	// TPM-only allowance: no RPM limit, so only tokens count.
	for i := 0; i < 500; i++ {
		if ok, _ := l.TryAdmit("global", "tpm-only", 0); !ok {
			t.Fatalf("request %d with 0 tokens refused on a TPM-only allowance", i)
		}
	}
	if ok, _ := l.TryAdmit("global", "tpm-only", 7_001); ok {
		t.Fatal("7001 tokens should exceed the 70% ceiling of a 10000 TPM allowance")
	}

	if ok, known := l.TryAdmit("asia-east1", "m", 1); ok || known {
		t.Fatalf("unknown target: got ok=%v known=%v, want false/false", ok, known)
	}
	if within, known := l.AdmitCritical("asia-east1", "m", 1); !within || known {
		t.Fatalf("unknown target critical: got within=%v known=%v, want true/false", within, known)
	}
}

func TestLimiter_ConcurrentAdmissionsNeverOverAdmit(t *testing.T) {
	l, err := governor.NewLimiter(limiterConfig(1000, 0, 1), newFakeClock().Now)
	if err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 100; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if ok, _ := l.TryAdmit("us-central1", "m", 1); ok {
					admitted.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if got := admitted.Load(); got != 700 {
		t.Fatalf("admitted %d under contention, want exactly 700", got)
	}
}

func TestCascadeEngine_DecideWithLimiter(t *testing.T) {
	cfg := limiterConfig(100, 0, 1)
	cfg.CustomPolicies = map[string]governor.CustomPolicy{
		"fb": {Cascade: []governor.CascadeStep{{TargetModel: "m", TargetRegion: "europe-west1"}}},
	}
	newEngine := func(t *testing.T) *governor.CascadeEngine {
		t.Helper()
		engine, err := governor.NewCascadeEngineFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		l, err := governor.NewLimiter(cfg, newFakeClock().Now)
		if err != nil {
			t.Fatal(err)
		}
		return engine.WithLimiter(l)
	}
	req := func(p governor.Priority) governor.Request {
		return governor.Request{Project: "p", Region: "us-central1", Model: "m", Priority: p, Policy: "fb"}
	}

	t.Run("no snapshot: limiter alone sheds best effort at the reserve", func(t *testing.T) {
		engine := newEngine(t)
		for i := 0; i < 70; i++ {
			if d := engine.Decide(nil, req(governor.Priority_PRIORITY_BEST_EFFORT)); d.IsDrop() {
				t.Fatalf("request %d shed early: %s", i, d.Reason)
			}
		}
		d := engine.Decide(nil, req(governor.Priority_PRIORITY_UNSPECIFIED))
		if !d.IsDrop() {
			t.Fatal("unlabelled request past the ceiling should be shed")
		}

		// Critical keeps flowing through the reserve and beyond it.
		for i := 0; i < 50; i++ {
			if d := engine.Decide(nil, req(governor.Priority_PRIORITY_CRITICAL)); d.IsDrop() {
				t.Fatalf("critical request %d shed: %s", i, d.Reason)
			}
		}
		if d := engine.Decide(nil, req(governor.Priority_PRIORITY_CRITICAL)); d.Reason == "" || d.IsDrop() {
			t.Fatalf("critical over allowance should be admitted with a warning, got %+v", d)
		}
	})

	t.Run("custom reroutes when the local primary bucket hits the reserve", func(t *testing.T) {
		engine := newEngine(t)
		for i := 0; i < 70; i++ {
			engine.Decide(nil, req(governor.Priority_PRIORITY_CUSTOM))
		}
		d := engine.Decide(nil, req(governor.Priority_PRIORITY_CUSTOM))
		if d.IsDrop() || d.ReplaceRegion != "europe-west1" {
			t.Fatalf("want reroute to europe-west1, got drop=%v region=%q (%s)", d.IsDrop(), d.ReplaceRegion, d.Reason)
		}
	})

	t.Run("full snapshot target does not consume local tokens", func(t *testing.T) {
		engine := newEngine(t)
		full := &pb.QuotaSnapshot{ProjectQuotas: map[string]*pb.ModelQuota{
			"p/us-central1/m": {MaxRpm: 1000, HeadroomRpm: 0, UtilizationRpm: 0.99},
		}}
		for i := 0; i < 200; i++ {
			if d := engine.Decide(full, req(governor.Priority_PRIORITY_BEST_EFFORT)); !d.IsDrop() {
				t.Fatal("best effort admitted on a full snapshot target")
			}
		}
		// The local bucket was never touched, so best effort fits once the snapshot clears.
		if d := engine.Decide(nil, req(governor.Priority_PRIORITY_BEST_EFFORT)); d.IsDrop() {
			t.Fatalf("local bucket was drained by shed requests: %s", d.Reason)
		}
	})

	t.Run("unknown target still fails open", func(t *testing.T) {
		engine := newEngine(t)
		r := req(governor.Priority_PRIORITY_BEST_EFFORT)
		r.Region = "asia-east1"
		if d := engine.Decide(nil, r); d.IsDrop() {
			t.Fatal("unknown target should be admitted optimistically")
		}
	})
}

func TestLimiter_ApplySharesAndReset(t *testing.T) {
	clk := newFakeClock()
	l, err := governor.NewLimiter(limiterConfig(100, 0, 2), clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	admitAll := func() int {
		n := 0
		for {
			if ok, _ := l.TryAdmit("us-central1", "m", 0); !ok {
				return n
			}
			n++
		}
	}

	// Growing the share raises the cap but does not hand out tokens
	// instantly (that could let replicas jointly exceed the allowance):
	// the balance stays at 50, and best effort stops at 30% of 80 = 24.
	l.ApplyShares([]*pb.TargetShare{{Target: "US-CENTRAL1/m", RpmFraction: 0.8, TpmFraction: 0.8}, {Target: "unknown/x", RpmFraction: 1}})
	if got := admitAll(); got != 26 {
		t.Fatalf("admitted %d right after growing the share, want 26", got)
	}
	// After a minute of refill the bucket is full at the new cap of 80; best
	// effort stops at 70% = 56.
	clk.Advance(time.Minute)
	if got := admitAll(); got != 56 {
		t.Fatalf("admitted %d with a refilled 80%% share, want 56", got)
	}

	// Shrinking clamps the balance; reset returns to the even 2-way split (50).
	l.ApplyShares([]*pb.TargetShare{{Target: "us-central1/m", RpmFraction: 0.1}})
	l.ResetShares()
	if rpm := availRPM(t, l); rpm > 50 {
		t.Fatalf("after reset available = %d, want <= 50", rpm)
	}
}

// availRPM probes how many requests fit using critical admissions, which do
// not stop at the reserve. It drains the bucket.
func availRPM(t *testing.T, l *governor.Limiter) int {
	t.Helper()
	n := 0
	for i := 0; i < 1000; i++ {
		within, _ := l.AdmitCritical("us-central1", "m", 0)
		if !within {
			return n
		}
		n++
	}
	return n
}

func TestLimiter_DrainDemandCountsAdmittedAndRejected(t *testing.T) {
	l, err := governor.NewLimiter(limiterConfig(10, 0, 1), newFakeClock().Now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ { // 7 admitted, 13 rejected
		l.TryAdmit("us-central1", "m", 100)
	}
	l.AdmitCritical("europe-west1", "m", 5)
	l.TryAdmit("asia-east1", "m", 1) // unknown: not counted

	got := map[string]*pb.TargetDemand{}
	for _, d := range l.DrainDemand() {
		got[d.GetTarget()] = d
	}
	if d := got["us-central1/m"]; d == nil || d.GetRequests() != 20 || d.GetTokens() != 2000 {
		t.Fatalf("us-central1 demand = %+v, want 20 requests / 2000 tokens", d)
	}
	if d := got["europe-west1/m"]; d == nil || d.GetRequests() != 1 || d.GetTokens() != 5 {
		t.Fatalf("europe-west1 demand = %+v", d)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected targets: %v", got)
	}
	if again := l.DrainDemand(); len(again) != 0 {
		t.Fatalf("second drain should be empty, got %v", again)
	}
}

func TestTokenBucket_SmallAllowanceKeepsReserve(t *testing.T) {
	tb := governor.NewTokenBucketWithClock(3, 0, newFakeClock().Now)
	admitted := 0
	for tb.TryAcquireAbove(0, 30) {
		admitted++
	}
	if admitted != 2 {
		t.Fatalf("best effort admitted %d of 3, want 2 (1 kept for critical)", admitted)
	}
	if !tb.TryAcquire(0) {
		t.Fatal("the reserved request should still be available to critical traffic")
	}
}
