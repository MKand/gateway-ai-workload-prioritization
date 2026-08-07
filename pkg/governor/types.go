package governor

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type ModelQuota struct {
	Model          string  `json:"model"`
	Region         string  `json:"region"`
	MaxTPM         int64   `json:"maxTPM"`
	MaxRPM         int64   `json:"maxRPM"`
	CurrentRPM     int64   `json:"currentRPM"`
	CurrentTPM     int64   `json:"currentTPM"`
	HeadroomRPM    int64   `json:"headroomRPM"`
	HeadroomTPM    int64   `json:"headroomTPM"`
	UtilizationRPM float64 `json:"utilizationRPM"`
	UtilizationTPM float64 `json:"utilizationTPM"`
}

type QuotaSnapshot struct {
	Quotas       map[string]*ModelQuota `json:"quotas"`
	LastSyncedAt time.Time              `json:"lastSyncedAt"`
}

type Decision struct {
	Drop          bool          `json:"drop"`
	ReplaceModel  string        `json:"replaceModel"`
	ReplaceRegion string        `json:"replaceRegion"`
	Reason        string        `json:"reason"`
	RetryAfter    time.Duration `json:"retryAfter"`
}

func (d *Decision) IsDrop() bool {
	return d.Drop
}

func (d Decision) IsForward() bool {
	return !d.Drop && d.ReplaceModel == "" && d.ReplaceRegion == ""
}

func (qs *QuotaSnapshot) GetQuota(region, model string) (*ModelQuota, error) {
	if qs == nil || qs.Quotas == nil || len(qs.Quotas) == 0 {
		return nil, errors.New("quota data not available")
	}

	regionLower := strings.ToLower(region)
	modelLower := strings.ToLower(model)

	q := qs.Quotas[fmt.Sprintf("%s/%s", regionLower, modelLower)]
	if q == nil {
		return nil, fmt.Errorf("no quota found for region %s and model %s", region, model)
	}
	return q, nil
}

type PriorityPolicy interface {
	Name() string
	Evaluate(quota *ModelQuota) Decision
}

type Config struct {
	ProjectIDs               []string      `json:"projectIds" yaml:"project_ids"`
	Regions                  []string      `json:"regions" yaml:"regions"`
	Models                   []string      `json:"models" yaml:"models"`
	PollInterval             time.Duration `json:"pollInterval" yaml:"poll_interval"`
	PollTimeout              time.Duration `json:"pollTimeout" yaml:"poll_timeout"`
	SafetyMargin             float64       `json:"safetyMargin" yaml:"safety_margin"`
	ShedThresholdBestEffort  float64       `json:"shedThresholdBestEffort" yaml:"shed_threshold_best_effort"`
}
