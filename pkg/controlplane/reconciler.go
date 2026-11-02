package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Reconciler struct {
	config        *governor.Config
	quotaClient   MetricsClient
	snapshotStore SnapshotStore
	logger        *slog.Logger
}

// SetLogger overrides the logger used by Reconciler. Passing nil restores slog.Default().
func (r *Reconciler) SetLogger(logger *slog.Logger) {
	r.logger = logger
}

func (r *Reconciler) log() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.Default()
}

func NewReconciler(config *governor.Config, client MetricsClient, store SnapshotStore) (*Reconciler, error) {
	if config == nil {
		return nil, errors.New("config cannot be nil")
	}
	if client == nil {
		return nil, errors.New("quota client cannot be nil")
	}
	if store == nil {
		return nil, errors.New("snapshot store cannot be nil")
	}
	return &Reconciler{
		config: config, quotaClient: client, snapshotStore: store,
	}, nil
}

func (r *Reconciler) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.config.PollInterval)
	defer ticker.Stop()

	if err := r.reconcile(ctx); err != nil {
		r.log().ErrorContext(ctx, "initial quota reconciliation failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.reconcile(ctx); err != nil {
				r.log().ErrorContext(ctx, "quota reconciliation failed", "err", err)
			}
		}
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

	if err := r.snapshotStore.Save(ctx, snapshot); err != nil {
		return err
	}
	r.log().DebugContext(ctx, "quota snapshot saved",
		"org_quotas", len(orgQuotas),
		"project_quotas", len(projectQuotas),
	)
	return nil
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
