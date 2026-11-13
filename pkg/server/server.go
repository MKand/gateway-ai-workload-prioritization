package server

import (
	"context"
	"errors"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	pb.UnimplementedQuotaDiscoveryServiceServer
	store  controlplane.SnapshotStore
	shares *controlplane.ShareTracker
	// bypass, if set, shrinks every share by the estimated traffic that
	// reaches Vertex AI without going through the governor.
	bypass *controlplane.BypassTracker
	now    func() time.Time
}

func NewServer(store controlplane.SnapshotStore) (*Server, error) {
	if store == nil {
		return nil, errors.New("snapshot store cannot be nil")
	}
	return &Server{
		store:  store,
		shares: controlplane.NewShareTracker(nil),
		now:    time.Now,
	}, nil
}

// WithBypassTracker enables bypass subtraction and returns the server.
func (s *Server) WithBypassTracker(b *controlplane.BypassTracker) *Server {
	s.bypass = b
	return s
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
	if s.bypass != nil {
		now := s.now()
		for _, d := range req.GetDemand() {
			s.bypass.RecordAdmitted(d.GetTarget(), now, d.GetAdmittedRequests(), d.GetAdmittedTokens())
		}
		shares = s.applyBypass(shares, active)
	}
	return &pb.ReportUsageResponse{Shares: shares, ActiveReplicas: active}, nil
}

// applyBypass scales every share by its target's allowance factor. Targets
// nobody has reported yet get an even share, so bypass traffic shrinks them
// too.
func (s *Server) applyBypass(shares []*pb.TargetShare, active int32) []*pb.TargetShare {
	byTarget := make(map[string]*pb.TargetShare, len(shares))
	for _, sh := range shares {
		byTarget[sh.GetTarget()] = sh
	}
	even := 1.0
	if active > 0 {
		even = 1 / float64(active)
	}
	out := make([]*pb.TargetShare, 0, len(byTarget))
	for _, target := range s.bypass.Targets() {
		sh, ok := byTarget[target]
		if !ok {
			sh = &pb.TargetShare{Target: target, RpmFraction: even, TpmFraction: even}
		}
		delete(byTarget, target)
		e := s.bypass.Estimate(target)
		out = append(out, &pb.TargetShare{
			Target:      target,
			RpmFraction: sh.GetRpmFraction() * e.RPMFactor,
			TpmFraction: sh.GetTpmFraction() * e.TPMFactor,
		})
	}
	// Targets without an allowance pass through unchanged.
	for _, sh := range shares {
		if _, left := byTarget[sh.GetTarget()]; left {
			out = append(out, sh)
		}
	}
	return out
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
