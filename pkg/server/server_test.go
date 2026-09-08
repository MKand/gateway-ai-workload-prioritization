package server

import (
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