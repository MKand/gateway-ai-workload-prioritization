package controlplane

import (
	"math"
	"testing"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
)

type shareClock struct{ t time.Time }

func (c *shareClock) now() time.Time { return c.t }

func demand(target string, req, tok int64) []*pb.TargetDemand {
	return []*pb.TargetDemand{{Target: target, Requests: req, Tokens: tok}}
}

func shareFor(t *testing.T, shares []*pb.TargetShare, target string) *pb.TargetShare {
	t.Helper()
	for _, s := range shares {
		if s.GetTarget() == target {
			return s
		}
	}
	t.Fatalf("no share for %q in %v", target, shares)
	return nil
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestShareTracker_SingleReplicaGetsEverything(t *testing.T) {
	tr := NewShareTracker(nil)
	shares, active, err := tr.Report("a", demand("us-central1/m", 10, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active = %d, want 1", active)
	}
	s := shareFor(t, shares, "us-central1/m")
	if !near(s.GetRpmFraction(), 1) || !near(s.GetTpmFraction(), 1) {
		t.Fatalf("share = %+v, want 1/1", s)
	}
}

func TestShareTracker_SplitsByDemandWithFloor(t *testing.T) {
	tr := NewShareTracker(nil)
	if _, _, err := tr.Report("busy", demand("us-central1/m", 80, 8000)); err != nil {
		t.Fatal(err)
	}
	quietShares, _, _ := tr.Report("quiet", demand("us-central1/m", 20, 2000))
	busyShares, active, _ := tr.Report("busy", demand("us-central1/m", 80, 8000))
	if active != 2 {
		t.Fatalf("active = %d, want 2", active)
	}
	busy := shareFor(t, busyShares, "us-central1/m").GetRpmFraction()
	quiet := shareFor(t, quietShares, "us-central1/m").GetRpmFraction()
	// floor 10% split 2 ways = 5% each; the other 90% split 80/20 = 72% / 18%.
	if !near(busy, 0.77) || !near(quiet, 0.23) {
		t.Fatalf("busy=%v quiet=%v, want 0.77 / 0.23", busy, quiet)
	}
}

func TestShareTracker_IdleReplicaKeepsFloor(t *testing.T) {
	tr := NewShareTracker(nil)
	tr.Report("busy", demand("us-central1/m", 100, 0))
	shares, _, _ := tr.Report("idle", nil)
	s := shareFor(t, shares, "us-central1/m")
	if !near(s.GetRpmFraction(), 0.05) {
		t.Fatalf("idle rpm share = %v, want the 5%% floor", s.GetRpmFraction())
	}
	// No token demand anywhere: TPM splits evenly.
	if !near(s.GetTpmFraction(), 0.5) {
		t.Fatalf("idle tpm share = %v, want 0.5 with no token demand", s.GetTpmFraction())
	}
}

func TestShareTracker_StaleReplicasExpire(t *testing.T) {
	clk := &shareClock{t: time.Unix(1_700_000_000, 0)}
	tr := NewShareTracker(clk.now)
	tr.Report("gone", demand("us-central1/m", 50, 0))
	clk.t = clk.t.Add(DefaultShareTTL + time.Second)
	shares, active, _ := tr.Report("alive", demand("us-central1/m", 50, 0))
	if active != 1 {
		t.Fatalf("active = %d, want 1 after the other replica went quiet", active)
	}
	if s := shareFor(t, shares, "us-central1/m"); !near(s.GetRpmFraction(), 1) {
		t.Fatalf("share = %v, want 1", s.GetRpmFraction())
	}
}

func TestShareTracker_SmoothsDemand(t *testing.T) {
	tr := NewShareTracker(nil)
	tr.Report("a", demand("t/m", 50, 0))
	tr.Report("b", demand("t/m", 50, 0))
	// "a" suddenly sends nothing: with smoothing its demand halves (25), it is
	// not dropped to zero in one step.
	shares, _, _ := tr.Report("a", nil)
	got := shareFor(t, shares, "t/m").GetRpmFraction()
	want := 0.05 + 0.9*25.0/75.0
	if !near(got, want) {
		t.Fatalf("share = %v, want %v", got, want)
	}
}

func TestShareTracker_Validation(t *testing.T) {
	tr := NewShareTracker(nil)
	if _, _, err := tr.Report("  ", nil); err == nil {
		t.Fatal("expected error for empty client id")
	}
	// Negative and empty-target entries are ignored, keys are normalised.
	shares, _, err := tr.Report("a", []*pb.TargetDemand{
		{Target: "US-Central1/M", Requests: 5},
		{Target: "", Requests: 5},
		{Target: "x/y", Requests: -3},
		nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	shareFor(t, shares, "us-central1/m")
}
