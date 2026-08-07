package controlplane

import (
	"context"
	"fmt"
	"strings"
)

type RawModelQuota struct {
	Model      string `json:"model"`
	Region     string `json:"region"`
	MaxTPM     int64  `json:"maxTPM"`
	MaxRPM     int64  `json:"maxRPM"`
	CurrentRPM int64  `json:"currentRPM"`
	CurrentTPM int64  `json:"currentTPM"`
}

type QuotaClient interface {
	FetchQuotas(ctx context.Context, projectIDs []string, regions []string, models []string) (map[string]*RawModelQuota, error)
}

type MockQuotaClient struct {
	Quotas map[string]*RawModelQuota
	Err    error
}

func NewMockQuotaClient() *MockQuotaClient {
	return &MockQuotaClient{}
}

func (mqc *MockQuotaClient) FetchQuotas(ctx context.Context, projectIDs []string, regions []string, models []string) (map[string]*RawModelQuota, error) {
	if mqc.Err != nil {
		return nil, mqc.Err
	}
	resp := map[string]*RawModelQuota{}

	for _, p := range projectIDs {
		for _, r := range regions {
			for _, m := range models {
				key := fmt.Sprintf("%s/%s/%s", strings.ToLower(p), strings.ToLower(r), strings.ToLower(m))
				rmq := &RawModelQuota{
					Region:     r,
					Model:      m,
					MaxRPM:     1000,
					MaxTPM:     2000000,
					CurrentRPM: 100,
					CurrentTPM: 50000,
				}
				resp[key] = rmq
			}
		}
	}
	return resp, nil
}

//TODO: implement GCPQuotaClient
