package controlplane

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
)

type SnapshotStore interface {
	Save(ctx context.Context, snapshot *pb.QuotaSnapshot) error
	Get(ctx context.Context) (*pb.QuotaSnapshot, error)
	Subscribe() (<-chan *pb.QuotaSnapshot, func())
}

type InMemoryStore struct {
	snapshot    atomic.Pointer[pb.QuotaSnapshot]
	subscribers map[chan *pb.QuotaSnapshot]struct{}
	mu          sync.Mutex
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		subscribers: make(map[chan *pb.QuotaSnapshot]struct{}),
	}
}

func (s *InMemoryStore) Save(ctx context.Context, snapshot *pb.QuotaSnapshot) error {
	if snapshot == nil {
		return errors.New("cannot save nil snapshot")
	}
	s.snapshot.Store(snapshot)

	// Send notifications to subscribers
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- snapshot:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- snapshot:
			default:
			}
		}
	}

	return nil
}

func (s *InMemoryStore) Get(ctx context.Context) (*pb.QuotaSnapshot, error) {
	qs := s.snapshot.Load()
	if qs == nil {
		return nil, errors.New("no quota snapshot available")
	}
	return qs, nil
}

func (s *InMemoryStore) Subscribe() (<-chan *pb.QuotaSnapshot, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ch := make(chan *pb.QuotaSnapshot, 1)
	s.subscribers[ch] = struct{}{}

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.subscribers, ch)
			close(ch)
		})
	}
	return ch, unsubscribe
}
