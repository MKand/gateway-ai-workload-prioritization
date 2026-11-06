package dataplane

import (
	"context"
	"log"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

const (
	// DefaultReportInterval is how often a replica reports its demand.
	DefaultReportInterval = 10 * time.Second

	// fallbackAfterFailures is how many consecutive failed reports it takes
	// before the replica drops back to the even split from dataplane_replicas.
	// Keeping the last shares for a while avoids flapping on a brief blip.
	fallbackAfterFailures = 3
)

// UsageReporter periodically sends this replica's demand to the control plane
// and resizes the local limiter to the demand-weighted share it gets back.
type UsageReporter struct {
	client   pb.QuotaDiscoveryServiceClient
	limiter  *governor.Limiter
	clientID string
	interval time.Duration

	failures int
}

// NewUsageReporter builds a reporter. interval <= 0 means DefaultReportInterval.
func NewUsageReporter(client pb.QuotaDiscoveryServiceClient, limiter *governor.Limiter, clientID string, interval time.Duration) *UsageReporter {
	if interval <= 0 {
		interval = DefaultReportInterval
	}
	return &UsageReporter{client: client, limiter: limiter, clientID: clientID, interval: interval}
}

// Run reports on every tick until ctx is cancelled.
func (r *UsageReporter) Run(ctx context.Context) error {
	if r.limiter == nil {
		return nil
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.ReportOnce(ctx)
		}
	}
}

// ReportOnce sends one report and applies the result. On repeated failure it
// falls back to the even split. It reports whether the call succeeded.
func (r *UsageReporter) ReportOnce(ctx context.Context) bool {
	callCtx, cancel := context.WithTimeout(ctx, r.interval)
	defer cancel()

	resp, err := r.client.ReportUsage(callCtx, &pb.ReportUsageRequest{
		ClientId: r.clientID,
		Demand:   r.limiter.DrainDemand(),
	})
	if err != nil {
		r.failures++
		if r.failures == fallbackAfterFailures {
			log.Printf("usage report failed %d times (%v); using the even split from dataplane_replicas", r.failures, err)
			r.limiter.ResetShares()
		}
		return false
	}
	r.failures = 0
	r.limiter.ApplyShares(resp.GetShares())
	return true
}
