package governor

import (
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
)

type PriorityPolicy interface {
	Name() string
	Evaluate(quota *pb.ModelQuota) pb.Decision
}

type Config struct {
	ProjectIDs              []string                 `json:"projectIds" yaml:"project_ids"`
	Regions                 []string                 `json:"regions" yaml:"regions"`
	Models                  []string                 `json:"models" yaml:"models"`
	PollInterval            time.Duration            `json:"pollInterval" yaml:"poll_interval"`
	PollTimeout             time.Duration            `json:"pollTimeout" yaml:"poll_timeout"`
	SafetyMarginPercent     int64                    `json:"safetyMargin" yaml:"safety_margin"`
	ShedThresholdBestEffort float64                  `json:"shedThresholdBestEffort" yaml:"shed_threshold_best_effort"`
	DefaultProjectLimits    map[string]pb.ModelLimit `json:"defaultProjectLimits" yaml:"default_project_limits"`
	DefaultOrgLimits        map[string]pb.ModelLimit `json:"defaultOrgLimits" yaml:"default_org_limits"`
}
