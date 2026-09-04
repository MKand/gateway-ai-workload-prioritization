package controlplane

import (
	"context"
	"errors"
	"fmt"
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
		fmt.Printf("Error when starting up reconciler: %s", err.Error())
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.reconcile(ctx); err != nil {
				fmt.Printf("Error when running reconcile loop: %s", err.Error())
			}
		}
	}
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
