package controlplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

type RawModelQuota struct {
	ProjectID  string `json:"projectID"`
	Model      string `json:"model"`
	Region     string `json:"region"`
	MaxTPM     int64  `json:"maxTPM"`
	MaxRPM     int64  `json:"maxRPM"`
	CurrentRPM int64  `json:"currentRPM"`
	CurrentTPM int64  `json:"currentTPM"`
}

func (mqc *RawModelQuota) GetKey() (string, error) {
	if mqc.ProjectID == "" || mqc.Region == "" || mqc.Model == "" {
		return "", fmt.Errorf("need string value for project=%s, region=%s and model=%s", mqc.ProjectID, mqc.Region, mqc.Model)
	}
	return fmt.Sprintf("%s/%s/%s", strings.ToLower(mqc.ProjectID), strings.ToLower(mqc.Region), strings.ToLower(mqc.Model)), nil
}

type QuotaClient interface {
	FetchQuotas(ctx context.Context, projectIDs []string, regions []string, models []string) (map[string]*RawModelQuota, error)
}

type MockQuotaClient struct {
	Quotas        map[string]*RawModelQuota
	projectLimits map[string]governor.ModelLimit
	orgLimits     map[string]governor.ModelLimit
	Err           error
}

func NewMockQuotaClient(projectLimits, orgLimits map[string]governor.ModelLimit) *MockQuotaClient {
	return &MockQuotaClient{
		projectLimits: projectLimits,
		orgLimits:     orgLimits,
	}
}

// resolveLimit finds the correct limit using family substring matching
func (mqc *MockQuotaClient) resolveLimit(limits map[string]governor.ModelLimit, modelName string) governor.ModelLimit {
	modelLower := strings.ToLower(modelName)
	// 1. Check Flash-Lite first (it contains "flash", so check it before "flash")
	if strings.Contains(modelLower, "flash-lite") {
		if limit, exists := limits["flash-lite"]; exists {
			return limit
		}
	}

	// 2. Check standard Flash
	if strings.Contains(modelLower, "flash") {
		if limit, exists := limits["flash"]; exists {
			return limit
		}
	}
	// 3. Check Pro
	if strings.Contains(modelLower, "pro") {
		if limit, exists := limits["pro"]; exists {
			return limit
		}
	}
	// Fallback default if no match is found
	return governor.ModelLimit{MaxRPM: 1000, MaxTPM: 2000000}
}

func (mqc *MockQuotaClient) FetchQuotas(ctx context.Context, projectIDs []string, regions []string, models []string) (map[string]*RawModelQuota, error) {
	if mqc.Err != nil {
		return nil, mqc.Err
	}
	resp := map[string]*RawModelQuota{}

	for _, r := range regions {
		for _, m := range models {
			// 1. Add Org-level quota (with org limits, 0 usage)
			orgLimit := mqc.resolveLimit(mqc.orgLimits, m)
			oKey := fmt.Sprintf("%s/%s", strings.ToLower(r), strings.ToLower(m))
			resp[oKey] = &RawModelQuota{
				Region:     r,
				Model:      m,
				MaxRPM:     orgLimit.MaxRPM,
				MaxTPM:     orgLimit.MaxTPM,
				CurrentRPM: 0,
				CurrentTPM: 0,
			}

			// 2. Add Project-level quotas (with project limits, simulated usage)
			for _, p := range projectIDs {
				projLimit := mqc.resolveLimit(mqc.projectLimits, m)
				prmq := &RawModelQuota{
					ProjectID:  p,
					Region:     r,
					Model:      m,
					MaxRPM:     projLimit.MaxRPM,
					MaxTPM:     projLimit.MaxTPM,
					CurrentRPM: 100,
					CurrentTPM: 50000,
				}
				pKey, err := prmq.GetKey()
				if err != nil {
					return nil, err
				}
				resp[pKey] = prmq
			}
		}
	}
	return resp, nil
}

//TODO: implement GCPQuotaClient
