package governor

import (
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

type PriorityPolicy interface {
	Name() string
	Evaluate(quota *pb.ModelQuota) Decision
}

type Config struct {
	OrgId                   string                  `json:"orgId" yaml:"org_id"`
	ProjectIDs              []string                `json:"projectIds" yaml:"project_ids"`
	Regions                 []string                `json:"regions" yaml:"regions"`
	Models                  []string                `json:"models" yaml:"models"`
	PollInterval            time.Duration           `json:"pollInterval" yaml:"poll_interval"`
	PollTimeout             time.Duration           `json:"pollTimeout" yaml:"poll_timeout"`
	SafetyMarginPercent     int64                   `json:"safetyMargin" yaml:"safety_margin"`
	ShedThresholdBestEffort float64                 `json:"shedThresholdBestEffort" yaml:"shed_threshold_best_effort"`
	DefaultProjectLimits    map[string]*ModelLimit  `json:"defaultProjectLimits" yaml:"default_project_limits"`
	DefaultOrgLimits        map[string]*ModelLimit  `json:"defaultOrgLimits" yaml:"default_org_limits"`
	CustomPolicies          map[string]CustomPolicy `json:"customPolicies,omitempty" yaml:"custom_policies,omitempty"`
}

// Priority represents the classification of incoming LLM traffic.
type Priority int32

const (
	Priority_PRIORITY_UNSPECIFIED Priority = iota
	Priority_PRIORITY_CRITICAL             //Unmodified
	Priority_PRIORITY_BEST_EFFORT          // Offline indexing, synthetic testing (shed early at >70%)
	Priority_PRIORITY_CUSTOM
)

type ModelLimit struct {
	MaxRpm int64 `json:"max_rpm" yaml:"max_rpm"`
	MaxTpm int64 `json:"max_tpm" yaml:"max_tpm"`
}

type Decision struct {
	Drop          bool                 `json:"drop"`                     // True if request must be shed with HTTP 429
	ReplaceModel  string               `json:"replace_model,omitempty"`  // If set, rewrite target model in URL path
	ReplaceRegion string               `json:"replace_region,omitempty"` // If set, rewrite target region in URL/host
	Reason        string               `json:"reason,omitempty"`         // Diagnostic reason for metrics/logs
	RetryAfter    *durationpb.Duration `json:"retry_after,omitempty"`    // Header value for HTTP 429
}

func (d *Decision) IsDrop() bool {
	return d.Drop
}

func (d *Decision) IsForward() bool {
	return !d.Drop && d.ReplaceModel == "" && d.ReplaceRegion == ""
}

type CascadeStep struct {
	TargetModel  string `json:"targetModel,omitempty" yaml:"target_model,omitempty"`
	TargetRegion string `json:"targetRegion,omitempty" yaml:"region,omitempty"`
}

type CustomPolicy struct {
	Cascade []CascadeStep `json:"cascade" yaml:"cascade"`
}
