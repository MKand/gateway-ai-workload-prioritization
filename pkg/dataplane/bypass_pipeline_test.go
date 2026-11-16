package dataplane

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/server"
)

// TestBypassShrinksLocalAllowance runs the real control-plane server and the
// real data-plane reporter over gRPC: once Cloud Monitoring shows traffic the
// governor did not admit, the data plane's best-effort budget shrinks.
func TestBypassShrinksLocalAllowance(t *testing.T) {
	cfg := &governor.Config{Allowances: map[string]*governor.ModelLimit{"us-central1/m": {MaxRpm: 100}}}

	bypass := controlplane.NewBypassTracker(cfg, nil)
	srv, err := server.NewServer(controlplane.NewInMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	srv.WithBypassTracker(bypass)

	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	pb.RegisterQuotaDiscoveryServiceServer(g, srv)
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	limiter, err := governor.NewLimiter(cfg, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	reporter := NewUsageReporter(pb.NewQuotaDiscoveryServiceClient(conn), limiter, "replica-1", time.Second)

	// Org-wide usage in the previous complete minute was 50 requests above
	// what this governor admitted: factor = 1 - 0.1 - 50/100 = 0.4.
	prev := time.Now().Truncate(time.Minute).Add(-time.Minute).Unix()
	bypass.Update(controlplane.UsageSeries{"us-central1/m": {prev: {Requests: 50}}})
	if !reporter.ReportOnce(context.Background()) {
		t.Fatal("report failed")
	}

	// Capacity 100 -> 40. The balance is clamped to 40 and best effort stops
	// at the 30% floor (12), so 28 best-effort requests fit instead of 70.
	if got := countAdmits(limiter); got != 28 {
		t.Fatalf("best effort admitted %d after bypass, want 28", got)
	}
}
