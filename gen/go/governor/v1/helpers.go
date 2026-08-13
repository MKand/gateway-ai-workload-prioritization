package governorv1

import (
	"errors"
	"fmt"
	"strings"
)

func (mqc *ModelQuota) GetKey() (string, error) {
	if mqc.Region == "" || mqc.Model == "" {
		return "", fmt.Errorf("need string value for region=%s and model=%s", mqc.Region, mqc.Model)
	}
	return fmt.Sprintf("%s/%s", strings.ToLower(mqc.Region), strings.ToLower(mqc.Model)), nil
}

func (d *Decision) IsDrop() bool {
	return d.Drop
}

func (d *Decision) IsForward() bool {
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
