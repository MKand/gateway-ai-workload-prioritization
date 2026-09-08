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
	store controlplane.SnapshotStore
}

func NewServer(store controlplane.SnapshotStore) (*Server, error) {
	if store == nil {
		return nil, errors.New("snapshot store cannot be nil")
	}
	return &Server{
		store: store,
	}, nil
}

func (s *Server) GetQuotaSnapshot(ctx context.Context, req *pb.GetQuotaSnapshotRequest) (*pb.QuotaSnapshot, error) {
	snapshot, err := s.store.Get(ctx)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "quota snapshot not available: %v", err)
	}
	return snapshot, nil

}

func (s *Server) StreamQuotas(req *pb.StreamQuotasRequest, stream pb.QuotaDiscoveryService_StreamQuotasServer) error {

	if initial, err := s.store.Get(stream.Context()); err == nil && initial != nil {
		if err := stream.Send(initial); err != nil {
			return err
		}
	}

	ch, unsubscribe := s.store.Subscribe()
	defer unsubscribe()

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
