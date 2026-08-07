package controlplane

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

type SnapshotStore interface {
	Save(ctx context.Context, snapshot *governor.QuotaSnapshot) error
	Get(ctx context.Context) (*governor.QuotaSnapshot, error)
}

type InMemoryStore struct {
	snapshot atomic.Pointer[governor.QuotaSnapshot]
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{}
}

func (s *InMemoryStore) Save(ctx context.Context, snapshot *governor.QuotaSnapshot) error {
	if snapshot == nil {
		return errors.New("cannot save nil snapshot")
	}
	s.snapshot.Store(snapshot)
	return nil
}

func (s *InMemoryStore) Get(ctx context.Context) (*governor.QuotaSnapshot, error) {
	qs := s.snapshot.Load()
	if qs == nil {
		return nil, errors.New("no quota snapshot available")
	}
	return qs, nil
}
