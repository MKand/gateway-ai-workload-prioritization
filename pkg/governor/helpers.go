package governor

import (
	"errors"
	"fmt"
	"strings"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
)

// GetOrgQuota returns the global Org-level quota for a region/model.
func GetOrgQuota(qs *pb.QuotaSnapshot, region, model string) (*pb.ModelQuota, error) {
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
func GetProjectQuota(qs *pb.QuotaSnapshot, project, region, model string) (*pb.ModelQuota, error) {
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
