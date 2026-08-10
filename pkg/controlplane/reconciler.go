package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

type Reconciler struct {
	config        *governor.Config
	quotaClient   QuotaClient
	snapshotStore SnapshotStore
}

func NewReconciler(config *governor.Config, client QuotaClient, store SnapshotStore) (*Reconciler, error) {
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
	rawQuotas, err := r.quotaClient.FetchQuotas(ctx, r.config.ProjectIDs, r.config.Regions, r.config.Models)
	if err != nil {
		return err
	}

	projectQuotas := make(map[string]*governor.ModelQuota)
	orgQuotas := make(map[string]*governor.ModelQuota)

	for _, raw := range rawQuotas {
		if raw.ProjectID == "" {
			// ==========================================
			// Org-Level Quota Limit (from Client Org Fetch)
			// ==========================================
			orgKey := fmt.Sprintf("%s/%s", strings.ToLower(raw.Region), strings.ToLower(raw.Model))
			if _, exists := orgQuotas[orgKey]; !exists {
				orgQuotas[orgKey] = &governor.ModelQuota{
					Model:  raw.Model,
					Region: raw.Region,
				}
			}
			// Set the Org-level limits directly (not aggregated/summed from projects)
			orgQuotas[orgKey].MaxRPM = raw.MaxRPM
			orgQuotas[orgKey].MaxTPM = raw.MaxTPM
		} else {
			// ==========================================
			// Project-Level Quota
			// ==========================================
			pKey, err := raw.GetKey()
			if err != nil {
				return err
			}

			// Calculate project-level headroom and utilization
			pUsableRPM := float64(raw.MaxRPM) * (1.0 - r.config.SafetyMargin)
			pHeadroomRPM := int64(pUsableRPM) - raw.CurrentRPM
			var pUtilRPM float64
			if raw.MaxRPM > 0 {
				pUtilRPM = float64(raw.CurrentRPM) / float64(raw.MaxRPM)
			}

			pUsableTPM := float64(raw.MaxTPM) * (1.0 - r.config.SafetyMargin)
			pHeadroomTPM := int64(pUsableTPM) - raw.CurrentTPM
			var pUtilTPM float64
			if raw.MaxTPM > 0 {
				pUtilTPM = float64(raw.CurrentTPM) / float64(raw.MaxTPM)
			}

			projectQuotas[pKey] = &governor.ModelQuota{
				ProjectID:      raw.ProjectID,
				Model:          raw.Model,
				Region:         raw.Region,
				MaxRPM:         raw.MaxRPM,
				MaxTPM:         raw.MaxTPM,
				CurrentRPM:     raw.CurrentRPM,
				CurrentTPM:     raw.CurrentTPM,
				HeadroomRPM:    pHeadroomRPM,
				HeadroomTPM:    pHeadroomTPM,
				UtilizationRPM: pUtilRPM,
				UtilizationTPM: pUtilTPM,
			}

			// Aggregate project usage into Org-level quota
			orgKey := fmt.Sprintf("%s/%s", strings.ToLower(raw.Region), strings.ToLower(raw.Model))
			if _, exists := orgQuotas[orgKey]; !exists {
				orgQuotas[orgKey] = &governor.ModelQuota{
					Model:  raw.Model,
					Region: raw.Region,
				}
			}
			orgQuotas[orgKey].CurrentRPM += raw.CurrentRPM
			orgQuotas[orgKey].CurrentTPM += raw.CurrentTPM
		}
	}

	// ==========================================
	// Post-Process Org-Level Quota Metrics
	// ==========================================
	for _, q := range orgQuotas {
		usableRPM := float64(q.MaxRPM) * (1.0 - r.config.SafetyMargin)
		q.HeadroomRPM = int64(usableRPM) - q.CurrentRPM
		if q.MaxRPM > 0 {
			q.UtilizationRPM = float64(q.CurrentRPM) / float64(q.MaxRPM)
		}

		usableTPM := float64(q.MaxTPM) * (1.0 - r.config.SafetyMargin)
		q.HeadroomTPM = int64(usableTPM) - q.CurrentTPM
		if q.MaxTPM > 0 {
			q.UtilizationTPM = float64(q.CurrentTPM) / float64(q.MaxTPM)
		}
	}

	// Save the dual-view snapshot
	snapshot := &governor.QuotaSnapshot{
		OrgQuotas:     orgQuotas,
		ProjectQuotas: projectQuotas,
		LastSyncedAt:  time.Now(),
	}

	return r.snapshotStore.Save(ctx, snapshot)
}
