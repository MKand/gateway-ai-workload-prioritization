package governor

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type ModelLimit struct {
	MaxRPM int64 `json:"maxRPM" yaml:"max_rpm"`
	MaxTPM int64 `json:"maxTPM" yaml:"max_tpm"`
}

type ModelQuota struct {
	ProjectID string `json:"projectID,omitempty"` // omitempty because it's blank for Org quotas

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

func (mqc *ModelQuota) GetKey() (string, error) {
	if mqc.Region == "" || mqc.Model == "" {
		return "", fmt.Errorf("need string value for region=%s and model=%s", mqc.Region, mqc.Model)
	}
	return fmt.Sprintf("%s/%s", strings.ToLower(mqc.Model), strings.ToLower(mqc.Model)), nil
}

type QuotaSnapshot struct {
	OrgQuotas     map[string]*ModelQuota `json:"orgQuotas"`     // Key: "region/model"
	ProjectQuotas map[string]*ModelQuota `json:"projectQuotas"` // Key: "project/region/model"
	LastSyncedAt  time.Time              `json:"lastSyncedAt"`
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

// GetOrgQuota returns the global Org-level quota for a region/model.
func (qs *QuotaSnapshot) GetOrgQuota(region, model string) (*ModelQuota, error) {
	if qs == nil || qs.OrgQuotas == nil {
		return nil, errors.New("org quota data not available")
	}
	key := fmt.Sprintf("%s/%s", strings.ToLower(region), strings.ToLower(model))
	q := qs.OrgQuotas[key]
	if q == nil {
		return nil, fmt.Errorf("no org quota found for region %s and model %s", region, model)
	}
	return q, nil
}

// GetProjectQuota returns the specific project-level quota.
func (qs *QuotaSnapshot) GetProjectQuota(project, region, model string) (*ModelQuota, error) {
	if qs == nil || qs.ProjectQuotas == nil {
		return nil, errors.New("project quota data not available")
	}
	key := fmt.Sprintf("%s/%s/%s", strings.ToLower(project), strings.ToLower(region), strings.ToLower(model))
	q := qs.ProjectQuotas[key]
	if q == nil {
		return nil, fmt.Errorf("no project quota found for project %s, region %s and model %s", project, region, model)
	}
	return q, nil
}

type PriorityPolicy interface {
	Name() string
	Evaluate(quota *ModelQuota) Decision
}

type Config struct {
	ProjectIDs              []string              `json:"projectIds" yaml:"project_ids"`
	Regions                 []string              `json:"regions" yaml:"regions"`
	Models                  []string              `json:"models" yaml:"models"`
	PollInterval            time.Duration         `json:"pollInterval" yaml:"poll_interval"`
	PollTimeout             time.Duration         `json:"pollTimeout" yaml:"poll_timeout"`
	SafetyMargin            float64               `json:"safetyMargin" yaml:"safety_margin"`
	ShedThresholdBestEffort float64               `json:"shedThresholdBestEffort" yaml:"shed_threshold_best_effort"`
	DefaultProjectLimits    map[string]ModelLimit `json:"defaultProjectLimits" yaml:"default_project_limits"`
	DefaultOrgLimits        map[string]ModelLimit `json:"defaultOrgLimits" yaml:"default_org_limits"`
}
