package server

import (
	"math"
	"time"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"context"
	"io"
	"net"
	"testing"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

func setupTestServer(t *testing.T, store controlplane.SnapshotStore) (pb.QuotaDiscoveryServiceClient, func()) {
	t.Helper()

	lis := bufconn.Listen(bufSize)
	s := grpc.NewServer()

	srv, err := NewServer(store)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	pb.RegisterQuotaDiscoveryServiceServer(s, srv)

	go func() {
		if err := s.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("Server exited with error: %v", err)
		}
	}()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("Failed to dial bufconn: %v", err)
	}

	client := pb.NewQuotaDiscoveryServiceClient(conn)

	teardown := func() {
		_ = conn.Close()
		s.GracefulStop()
		_ = lis.Close()
	}

	return client, teardown
}

func TestNewServer_Validation(t *testing.T) {
	_, err := NewServer(nil)
	if err == nil {
		t.Errorf("expected error when store is nil, got nil")
	}
}

func TestServer_GetQuotaSnapshot(t *testing.T) {
	store := controlplane.NewInMemoryStore()
	client, cleanup := setupTestServer(t, store)
	defer cleanup()

	ctx := context.Background()

	// 1. Initial Get with empty store should return NotFound
	_, err := client.GetQuotaSnapshot(ctx, &pb.GetQuotaSnapshotRequest{})
	if err == nil {
		t.Fatalf("expected error on empty store, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.NotFound {
		t.Errorf("expected NotFound status code, got %v", err)
	}

	// 2. Save a snapshot and verify Get returns it
	snap := &pb.QuotaSnapshot{
		OrgQuotas: map[string]*pb.ModelQuota{
			"us-central1/gemini-3.5-flash": {MaxRpm: 150},
		},
	}
	if err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	retrieved, err := client.GetQuotaSnapshot(ctx, &pb.GetQuotaSnapshotRequest{})
	if err != nil {
		t.Fatalf("GetQuotaSnapshot failed unexpectedly: %v", err)
	}
	if retrieved.OrgQuotas["us-central1/gemini-3.5-flash"].MaxRpm != 150 {
		t.Errorf("expected MaxRpm 150, got %d", retrieved.OrgQuotas["us-central1/gemini-3.5-flash"].MaxRpm)
	}
}

func TestServer_StreamQuotas(t *testing.T) {
	store := controlplane.NewInMemoryStore()
	ctx := context.Background()

	// 1. Save initial snapshot
	initialSnap := &pb.QuotaSnapshot{
		OrgQuotas: map[string]*pb.ModelQuota{
			"us-central1/gemini-3.5-flash": {MaxRpm: 100},
		},
	}
	if err := store.Save(ctx, initialSnap); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	client, cleanup := setupTestServer(t, store)
	defer cleanup()

	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	stream, err := client.StreamQuotas(streamCtx, &pb.StreamQuotasRequest{ClientId: "dataplane-1"})
	if err != nil {
		t.Fatalf("StreamQuotas failed: %v", err)
	}

	// 2. Verify initial snapshot received immediately
	msg1, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() for initial snapshot failed: %v", err)
	}
	if msg1.OrgQuotas["us-central1/gemini-3.5-flash"].MaxRpm != 100 {
		t.Errorf("expected MaxRpm 100, got %d", msg1.OrgQuotas["us-central1/gemini-3.5-flash"].MaxRpm)
	}

	// 3. Save a second snapshot and verify it is pushed through stream
	secondSnap := &pb.QuotaSnapshot{
		OrgQuotas: map[string]*pb.ModelQuota{
			"us-central1/gemini-3.5-flash": {MaxRpm: 250},
		},
	}
	if err := store.Save(ctx, secondSnap); err != nil {
		t.Fatalf("Save second snapshot failed: %v", err)
	}

	msg2, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() for updated snapshot failed: %v", err)
	}
	if msg2.OrgQuotas["us-central1/gemini-3.5-flash"].MaxRpm != 250 {
		t.Errorf("expected MaxRpm 250, got %d", msg2.OrgQuotas["us-central1/gemini-3.5-flash"].MaxRpm)
	}

	// 4. Cancel stream context and verify clean termination
	cancelStream()
	_, err = stream.Recv()
	if err != nil && err != io.EOF {
		st, ok := status.FromError(err)
		if ok && st.Code() != codes.Canceled {
			t.Errorf("expected Canceled status or EOF, got %v", err)
		}
	}
}
func TestServer_ReportUsage(t *testing.T) {
	client, cleanup := setupTestServer(t, controlplane.NewInMemoryStore())
	defer cleanup()
	ctx := context.Background()

	if _, err := client.ReportUsage(ctx, &pb.ReportUsageRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty client id: got %v, want InvalidArgument", err)
	}

	if _, err := client.ReportUsage(ctx, &pb.ReportUsageRequest{ClientId: "a", Demand: []*pb.TargetDemand{{Target: "t/m", Requests: 30}}}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.ReportUsage(ctx, &pb.ReportUsageRequest{ClientId: "b", Demand: []*pb.TargetDemand{{Target: "t/m", Requests: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetActiveReplicas() != 2 || len(resp.GetShares()) != 1 {
		t.Fatalf("unexpected response %+v", resp)
	}
	// 5% floor + 90% * 10/40.
	if got := resp.GetShares()[0].GetRpmFraction(); got < 0.2749 || got > 0.2751 {
		t.Fatalf("b's share = %v, want 0.275", got)
	}
}

func TestServer_ReportUsageAppliesBypass(t *testing.T) {
	cfg := &governor.Config{Allowances: map[string]*governor.ModelLimit{
		"us-central1/m": {MaxRpm: 1000},
		"global/m":      {MaxRpm: 1000},
	}}
	now := time.Unix(1_700_000_000, 0).Truncate(time.Minute).Add(5*time.Minute + 30*time.Second)
	clock := func() time.Time { return now }
	bypass := controlplane.NewBypassTracker(cfg, clock)
	srv, err := NewServer(controlplane.NewInMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	srv.WithBypassTracker(bypass)
	srv.now = clock

	// Minute 4: the governor admits 400 requests, Monitoring later sees 600.
	minute4 := now.Truncate(time.Minute).Add(-time.Minute)
	srv.now = func() time.Time { return minute4.Add(10 * time.Second) }
	if _, err := srv.ReportUsage(context.Background(), &pb.ReportUsageRequest{ClientId: "a", Demand: []*pb.TargetDemand{
		{Target: "us-central1/m", Requests: 500, AdmittedRequests: 400},
	}}); err != nil {
		t.Fatal(err)
	}
	srv.now = clock
	bypass.Update(controlplane.UsageSeries{"us-central1/m": {minute4.Unix(): {Requests: 600}}})

	resp, err := srv.ReportUsage(context.Background(), &pb.ReportUsageRequest{ClientId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*pb.TargetShare{}
	for _, s := range resp.GetShares() {
		got[s.GetTarget()] = s
	}
	// Single replica (share 1) x factor (1 - 0.1 margin - 200/1000 bypass) = 0.7.
	if s := got["us-central1/m"]; s == nil || math.Abs(s.GetRpmFraction()-0.7) > 1e-9 {
		t.Fatalf("us-central1 share = %+v, want rpm 0.7", s)
	}
	// global has no demand yet: it still gets an (even) share, scaled by the
	// margin-only factor 0.9.
	if s := got["global/m"]; s == nil || math.Abs(s.GetRpmFraction()-0.9) > 1e-9 {
		t.Fatalf("global share = %+v, want rpm 0.9", s)
	}
}
