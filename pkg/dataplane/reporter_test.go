package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"google.golang.org/grpc"
)

// fixedClock never advances, so bucket balances only change when the test
// admits requests.
func fixedClock() time.Time { return time.Unix(1_700_000_000, 0) }

type fakeReportClient struct {
	pb.QuotaDiscoveryServiceClient
	err  error
	resp *pb.ReportUsageResponse
	last *pb.ReportUsageRequest
}

func (f *fakeReportClient) ReportUsage(_ context.Context, in *pb.ReportUsageRequest, _ ...grpc.CallOption) (*pb.ReportUsageResponse, error) {
	f.last = in
	return f.resp, f.err
}

func reporterLimiter(t *testing.T) *governor.Limiter {
	t.Helper()
	replicas := 2
	l, err := governor.NewLimiter(&governor.Config{
		Allowances:        map[string]*governor.ModelLimit{"us-central1/m": {MaxRpm: 100}},
		DataplaneReplicas: &replicas,
	}, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func countAdmits(l *governor.Limiter) int {
	n := 0
	for {
		if ok, _ := l.TryAdmit("us-central1", "m", 0); !ok {
			return n
		}
		n++
	}
}

func TestUsageReporter_AppliesShares(t *testing.T) {
	l := reporterLimiter(t)
	l.TryAdmit("us-central1", "m", 40)
	client := &fakeReportClient{resp: &pb.ReportUsageResponse{
		Shares: []*pb.TargetShare{{Target: "us-central1/m", RpmFraction: 0.9, TpmFraction: 0.9}},
	}}
	r := NewUsageReporter(client, l, "replica-1", 0)
	if !r.ReportOnce(context.Background()) {
		t.Fatal("report should succeed")
	}
	if client.last.GetClientId() != "replica-1" || len(client.last.GetDemand()) != 1 || client.last.GetDemand()[0].GetTokens() != 40 {
		t.Fatalf("unexpected request %+v", client.last)
	}
	// The cap grows from 50 to 90 but the balance stays 49 (no instant
	// top-up); best effort now stops at the 30% floor of 90 = 27.
	if got := countAdmits(l); got != 22 {
		t.Fatalf("admitted %d after a 90%% share, want 22", got)
	}
}

func TestUsageReporter_FallsBackAfterRepeatedFailures(t *testing.T) {
	l := reporterLimiter(t)
	client := &fakeReportClient{resp: &pb.ReportUsageResponse{
		Shares: []*pb.TargetShare{{Target: "us-central1/m", RpmFraction: 0.1}},
	}}
	r := NewUsageReporter(client, l, "replica-1", 0)
	r.ReportOnce(context.Background()) // shrink to 10

	client.err = errors.New("control plane down")
	for i := 0; i < fallbackAfterFailures-1; i++ {
		r.ReportOnce(context.Background())
	}
	if got := countAdmits(l); got != 7 {
		t.Fatalf("admitted %d before fallback, want 7 (last share kept)", got)
	}
	r.ReportOnce(context.Background()) // third failure: back to the even split, cap 100/2 = 50
	// Balance is 3 (10 - 7) and the floor is now 15, so nothing more fits
	// until refill; the point is the cap went back up rather than staying at 10.
	if got := countAdmits(l); got != 0 {
		t.Fatalf("admitted %d right after fallback, want 0", got)
	}
}
