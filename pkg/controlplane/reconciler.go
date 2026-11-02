package controlplane

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultJitterFraction       = 0.10
	defaultMaxBackoffMultiplier = 8
	maxBackoffShift             = 6
)

// ReconcilerOption configures optional Reconciler polling and backoff behavior.
type ReconcilerOption func(*Reconciler)

// WithJitterFraction sets the polling jitter fraction in [0.0, 1.0].
// Steady-state intervals vary within [1-f, 1+f] of PollInterval, and backoff
// delays vary within [1-f, 1.0] of the capped exponential delay.
func WithJitterFraction(fraction float64) ReconcilerOption {
	return func(r *Reconciler) {
		r.jitterFraction = fraction
	}
}

// WithMaxBackoff sets the upper bound on exponential backoff between failed
// reconciliation attempts. When unset or zero, defaults to 8 * PollInterval.
func WithMaxBackoff(d time.Duration) ReconcilerOption {
	return func(r *Reconciler) {
		r.maxBackoff = d
		r.hasCustomMaxBackoff = true
	}
}

// WithStartupJitter controls whether Start waits a random initial delay in
// [0, jitterFraction * PollInterval) before the first poll so replicas
// booting simultaneously do not call GCP APIs on the same millisecond.
func WithStartupJitter(enabled bool) ReconcilerOption {
	return func(r *Reconciler) {
		r.startupJitter = enabled
	}
}

type Reconciler struct {
	config              *governor.Config
	quotaClient         MetricsClient
	snapshotStore       SnapshotStore
	jitterFraction      float64
	maxBackoff          time.Duration
	hasCustomMaxBackoff bool
	startupJitter       bool
	sleepFn             func(context.Context, time.Duration) error
	randFloat64         func() float64
}

func NewReconciler(config *governor.Config, client MetricsClient, store SnapshotStore, opts ...ReconcilerOption) (*Reconciler, error) {
	if config == nil {
		return nil, errors.New("config cannot be nil")
	}
	if client == nil {
		return nil, errors.New("quota client cannot be nil")
	}
	if store == nil {
		return nil, errors.New("snapshot store cannot be nil")
	}
	r := &Reconciler{
		config:         config,
		quotaClient:    client,
		snapshotStore:  store,
		jitterFraction: defaultJitterFraction,
		startupJitter:  true,
		sleepFn:        sleepWithContext,
		randFloat64:    rand.Float64,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	if math.IsNaN(r.jitterFraction) || math.IsInf(r.jitterFraction, 0) || r.jitterFraction < 0 || r.jitterFraction > 1.0 {
		return nil, fmt.Errorf("jitter fraction must be in [0.0, 1.0], got %v", r.jitterFraction)
	}
	if r.hasCustomMaxBackoff && r.maxBackoff < 0 {
		return nil, fmt.Errorf("max backoff cannot be negative, got %v", r.maxBackoff)
	}
	return r, nil
}

func (r *Reconciler) Start(ctx context.Context) error {
	base := r.pollInterval()
	if r.startupJitter && r.jitterFraction > 0 {
		startupDelay := time.Duration(float64(base) * r.jitterFraction * r.randFloat64())
		if startupDelay > 0 {
			if err := r.sleepFn(ctx, startupDelay); err != nil {
				return err
			}
		}
	}

	consecutiveFailures := 0
	for {
		if err := r.reconcile(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			consecutiveFailures++
			fmt.Fprintf(os.Stderr, "reconciler poll failed (attempt %d): %v\n", consecutiveFailures, err)
		} else {
			consecutiveFailures = 0
		}

		if err := r.sleepFn(ctx, r.nextDelay(consecutiveFailures)); err != nil {
			return err
		}
	}
}

func (r *Reconciler) pollInterval() time.Duration {
	if r.config != nil && r.config.PollInterval > 0 {
		return r.config.PollInterval
	}
	return governor.MinPollInterval
}

func (r *Reconciler) nextDelay(consecutiveFailures int) time.Duration {
	base := r.pollInterval()
	if consecutiveFailures <= 0 {
		multiplier := 1.0 + r.jitterFraction*(2.0*r.randFloat64()-1.0)
		return time.Duration(float64(base) * multiplier)
	}

	maxDelay := r.maxBackoff
	if !r.hasCustomMaxBackoff || maxDelay <= 0 {
		maxDelay = defaultMaxBackoffMultiplier * base
	}
	if maxDelay < base {
		maxDelay = base
	}

	shift := consecutiveFailures
	if shift > maxBackoffShift {
		shift = maxBackoffShift
	}
	capped := base << shift
	if capped <= 0 || capped > maxDelay {
		capped = maxDelay
	}
	multiplier := 1.0 - r.jitterFraction*r.randFloat64()
	return time.Duration(float64(capped) * multiplier)
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ReconcileOnce runs a single fetch-and-store cycle. It is used by the
// control plane's --once mode to validate quota/usage reads against real GCP.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	return r.reconcile(ctx)
}

func (r *Reconciler) reconcile(ctx context.Context) error {
	rawQuotas, err := r.quotaClient.FetchMetrics(ctx, r.config.ProjectIDs, r.config.Regions, r.config.Models)
	if err != nil {
		return err
	}

	projectQuotas := make(map[string]*pb.ModelQuota)
	orgQuotas := make(map[string]*pb.ModelQuota)

	for _, raw := range rawQuotas {
		if raw.ProjectId == "" {
			// ==========================================
			// Org-Level Quota Limit (from Client Org Fetch)
			// ==========================================
			orgKey := fmt.Sprintf("%s/%s", strings.ToLower(raw.Region), strings.ToLower(raw.Model))
			if _, exists := orgQuotas[orgKey]; !exists {
				orgQuotas[orgKey] = &pb.ModelQuota{
					Model:  raw.Model,
					Region: raw.Region,
				}
			}
			orgQuotas[orgKey].MaxRpm = raw.MaxRpm
			orgQuotas[orgKey].MaxTpm = raw.MaxTpm
		} else {
			// ==========================================
			// Project-Level Quota
			// ==========================================
			pKey, err := raw.GetKey()
			if err != nil {
				return err
			}

			q := &pb.ModelQuota{
				ProjectId:  raw.ProjectId,
				Model:      raw.Model,
				Region:     raw.Region,
				MaxRpm:     raw.MaxRpm,
				MaxTpm:     raw.MaxTpm,
				CurrentRpm: raw.CurrentRpm,
				CurrentTpm: raw.CurrentTpm,
			}
			populateQuotaMetrics(q, r.config.SafetyMarginPercent)
			projectQuotas[pKey] = q

			orgKey := fmt.Sprintf("%s/%s", strings.ToLower(raw.Region), strings.ToLower(raw.Model))
			if _, exists := orgQuotas[orgKey]; !exists {
				orgQuotas[orgKey] = &pb.ModelQuota{
					Model:  raw.Model,
					Region: raw.Region,
				}
			}
			orgQuotas[orgKey].CurrentRpm += raw.CurrentRpm
			orgQuotas[orgKey].CurrentTpm += raw.CurrentTpm
		}
	}

	// ==========================================
	// Post-Process Org-Level Quota Metrics
	// ==========================================
	for _, q := range orgQuotas {
		populateQuotaMetrics(q, r.config.SafetyMarginPercent)
	}

	snapshot := &pb.QuotaSnapshot{
		OrgQuotas:     orgQuotas,
		ProjectQuotas: projectQuotas,
		LastSyncedAt:  timestamppb.Now(),
	}

	return r.snapshotStore.Save(ctx, snapshot)
}

// computeHeadroomMetric calculates usable headroom and utilization ratio for a quota counter.
func computeHeadroomMetric(max, current, safetyMarginPercent int64) (headroom int64, utilization float64) {
	usable := (max * (100 - safetyMarginPercent)) / 100
	headroom = usable - current
	if max > 0 {
		utilization = float64(current) / float64(max)
	}
	return headroom, utilization
}

// populateQuotaMetrics populates Headroom and Utilization for both RPM and TPM on a ModelQuota.
func populateQuotaMetrics(q *pb.ModelQuota, safetyMarginPercent int64) {
	q.HeadroomRpm, q.UtilizationRpm = computeHeadroomMetric(q.MaxRpm, q.CurrentRpm, safetyMarginPercent)
	q.HeadroomTpm, q.UtilizationTpm = computeHeadroomMetric(q.MaxTpm, q.CurrentTpm, safetyMarginPercent)
}
