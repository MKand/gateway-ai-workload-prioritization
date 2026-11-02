// Package dataplane implements the request-path side of the governor: a local
// cache of the latest QuotaSnapshot streamed from the Control Plane, and an
// Envoy ext_proc server that turns that snapshot into request header mutations.
package dataplane

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
)

const (
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 10 * time.Second
)

// SnapshotCache holds the most recent QuotaSnapshot. Reads are lock-free so the
// request hot path never blocks on the control-plane stream.
type SnapshotCache struct {
	current atomic.Pointer[pb.QuotaSnapshot]
	logger  *slog.Logger
}

// NewSnapshotCache returns an empty cache. Get returns nil until the first
// snapshot arrives, which the evaluator treats as "fail open".
func NewSnapshotCache() *SnapshotCache {
	return &SnapshotCache{}
}

// SetLogger overrides the logger used by SnapshotCache. Passing nil restores slog.Default().
func (c *SnapshotCache) SetLogger(logger *slog.Logger) {
	c.logger = logger
}

func (c *SnapshotCache) log() *slog.Logger {
	if c.logger != nil {
		return c.logger
	}
	return slog.Default()
}

// Get returns the latest snapshot, or nil if none has been received yet.
func (c *SnapshotCache) Get() *pb.QuotaSnapshot {
	return c.current.Load()
}

// Set replaces the cached snapshot.
func (c *SnapshotCache) Set(s *pb.QuotaSnapshot) {
	c.current.Store(s)
}

// Run subscribes to the Control Plane's StreamQuotas RPC and keeps the cache
// up to date. It reconnects with exponential backoff and only returns when ctx
// is cancelled. The last known snapshot is kept while disconnected.
func (c *SnapshotCache) Run(ctx context.Context, client pb.QuotaDiscoveryServiceClient, clientID string) error {
	backoff := initialBackoff
	for {
		received, err := c.streamOnce(ctx, client, clientID)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if received {
			backoff = initialBackoff
		}
		c.log().WarnContext(ctx, "quota stream disconnected",
			"client_id", clientID,
			"retry_in", backoff,
			"err", err,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// streamOnce consumes a single StreamQuotas stream until it fails. It reports
// whether at least one snapshot was received so Run can reset its backoff.
func (c *SnapshotCache) streamOnce(ctx context.Context, client pb.QuotaDiscoveryServiceClient, clientID string) (bool, error) {
	stream, err := client.StreamQuotas(ctx, &pb.StreamQuotasRequest{ClientId: clientID})
	if err != nil {
		return false, err
	}
	received := false
	for {
		snap, err := stream.Recv()
		if err != nil {
			return received, err
		}
		if snap == nil {
			return received, errors.New("received nil snapshot")
		}
		c.Set(snap)
		received = true
	}
}
