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

			pUsableRPM := (raw.MaxRpm * (100 - r.config.SafetyMarginPercent)) / 100
			pHeadroomRPM := pUsableRPM - raw.CurrentRpm
			var pUtilRPM float64
			if raw.MaxRpm > 0 {
				pUtilRPM = float64(raw.CurrentRpm) / float64(raw.MaxRpm)
			}

			pUsableTPM := (raw.MaxTpm * (100 - r.config.SafetyMarginPercent)) / 100
			pHeadroomTPM := int64(pUsableTPM) - raw.CurrentTpm
			var pUtilTPM float64
			if raw.MaxTpm > 0 {
				pUtilTPM = float64(raw.CurrentTpm) / float64(raw.MaxTpm)
			}

			projectQuotas[pKey] = &pb.ModelQuota{
				ProjectId:      raw.ProjectId,
				Model:          raw.Model,
				Region:         raw.Region,
				MaxRpm:         raw.MaxRpm,
				MaxTpm:         raw.MaxTpm,
				CurrentRpm:     raw.CurrentRpm,
				CurrentTpm:     raw.CurrentTpm,
				HeadroomRpm:    pHeadroomRPM,
				HeadroomTpm:    pHeadroomTPM,
				UtilizationRpm: pUtilRPM,
				UtilizationTpm: pUtilTPM,
			}

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
		usableRPM := (q.MaxRpm * (100 - r.config.SafetyMarginPercent)) / 100
		q.HeadroomRpm = usableRPM - q.CurrentRpm
		if q.MaxRpm > 0 {
			q.UtilizationRpm = float64(q.CurrentRpm) / float64(q.MaxRpm)
		}

		usableTPM := (q.MaxTpm * (100 - r.config.SafetyMarginPercent)) / 100
		q.HeadroomTpm = usableTPM - q.CurrentTpm
		if q.MaxTpm > 0 {
			q.UtilizationTpm = float64(q.CurrentTpm) / float64(q.MaxTpm)
		}
	}

	snapshot := &pb.QuotaSnapshot{
		OrgQuotas:     orgQuotas,
		ProjectQuotas: projectQuotas,
		LastSyncedAt:  timestamppb.Now(),
	}

	return r.snapshotStore.Save(ctx, snapshot)
}
