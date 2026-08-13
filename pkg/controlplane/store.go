package controlplane

import (
	"context"
	"errors"
	"sync/atomic"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
)

type SnapshotStore interface {
	Save(ctx context.Context, snapshot *pb.QuotaSnapshot) error
	Get(ctx context.Context) (*pb.QuotaSnapshot, error)
}

type InMemoryStore struct {
	snapshot atomic.Pointer[pb.QuotaSnapshot]
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{}
}

func (s *InMemoryStore) Save(ctx context.Context, snapshot *pb.QuotaSnapshot) error {
	if snapshot == nil {
		return errors.New("cannot save nil snapshot")
	}
	s.snapshot.Store(snapshot)
	return nil
}

func (s *InMemoryStore) Get(ctx context.Context) (*pb.QuotaSnapshot, error) {
	qs := s.snapshot.Load()
	if qs == nil {
		return nil, errors.New("no quota snapshot available")
	}
	return qs, nil
}
