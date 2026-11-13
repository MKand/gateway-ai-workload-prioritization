package server

import (
	"context"
	"errors"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	pb.UnimplementedQuotaDiscoveryServiceServer
	store  controlplane.SnapshotStore
	shares *controlplane.ShareTracker
}

func NewServer(store controlplane.SnapshotStore) (*Server, error) {
	if store == nil {
		return nil, errors.New("snapshot store cannot be nil")
	}
	return &Server{
		store:  store,
		shares: controlplane.NewShareTracker(nil),
	}, nil
}

// ShareTracker returns the server's ShareTracker.
func (s *Server) ShareTracker() *controlplane.ShareTracker { return s.shares }

// WithShareTracker replaces the server's ShareTracker (for example one with a
// fake clock in tests) and returns the server.
func (s *Server) WithShareTracker(t *controlplane.ShareTracker) *Server {
	s.shares = t
	return s
}

// ReportUsage records a data plane replica's recent demand and returns its
// demand-weighted share of each allowance.
func (s *Server) ReportUsage(ctx context.Context, req *pb.ReportUsageRequest) (*pb.ReportUsageResponse, error) {
	shares, active, err := s.shares.Report(req.GetClientId(), req.GetDemand())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return &pb.ReportUsageResponse{Shares: shares, ActiveReplicas: active}, nil
}

func (s *Server) GetQuotaSnapshot(ctx context.Context, req *pb.GetQuotaSnapshotRequest) (*pb.QuotaSnapshot, error) {
	snapshot, err := s.store.Get(ctx)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "quota snapshot not available: %v", err)
	}
	return snapshot, nil

}

func (s *Server) StreamQuotas(req *pb.StreamQuotasRequest, stream pb.QuotaDiscoveryService_StreamQuotasServer) error {
	// Subscribe before reading the initial snapshot so a Save that lands in
	// between is not lost.
	ch, unsubscribe := s.store.Subscribe()
	defer unsubscribe()

	if initial, err := s.store.Get(stream.Context()); err == nil && initial != nil {
		if err := stream.Send(initial); err != nil {
			return err
		}
	}

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()

		case snapshot, ok := <-ch:
			if !ok {
				return status.Errorf(codes.Unavailable, "quota subscription closed unexpectedly")
			}
			if err := stream.Send(snapshot); err != nil {
				return err
			}
		}
	}
}
