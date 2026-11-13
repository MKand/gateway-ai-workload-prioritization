package governor

import (
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
)

type Config struct {
	ProjectIDs           []string               `json:"projectIds" yaml:"project_ids"`
	Regions              []string               `json:"regions" yaml:"regions"`
	Models               []string               `json:"models" yaml:"models"`
	PollInterval         time.Duration          `json:"pollInterval" yaml:"poll_interval"`
	PollTimeout          time.Duration          `json:"pollTimeout" yaml:"poll_timeout"`
	SafetyMarginPercent  int64                  `json:"safetyMargin" yaml:"safety_margin"`
	DefaultProjectLimits map[string]*ModelLimit `json:"defaultProjectLimits" yaml:"default_project_limits"`

	// Allowances is the org's DSQ allowance per "location/model" (for example
	// "global/gemini-3.5-flash" or "us-central1/gemini-3.5-flash"). Customers
	// cannot query this number, so it is configured up front. Keys are
	// normalised to lower case by LoadConfig.
	Allowances map[string]*ModelLimit `json:"allowances" yaml:"allowances"`

	// CriticalReservePercent is the share of each allowance that only critical
	// traffic may use. Best-effort and custom traffic stop at
	// 100 - CriticalReservePercent. nil means DefaultCriticalReservePercent.
	CriticalReservePercent *int64 `json:"criticalReservePercent,omitempty" yaml:"critical_reserve_percent,omitempty"`

	// DataplaneReplicas is the number of data plane replicas the allowance is
	// split across when the control plane cannot report the live count.
	// nil means 1.
	DataplaneReplicas *int `json:"dataplaneReplicas,omitempty" yaml:"dataplane_replicas,omitempty"`

	// DefaultRequestTokens is the input-token estimate charged for a request
	// that has neither Content-Length nor X-Prompt-Tokens. 0 means
	// DefaultRequestTokensEstimate.
	DefaultRequestTokens int64 `json:"defaultRequestTokens,omitempty" yaml:"default_request_tokens,omitempty"`

	CustomPolicies map[string]CustomPolicy `json:"customPolicies,omitempty" yaml:"custom_policies,omitempty"`
}

// RequestTokens returns the token estimate for requests without size hints.
func (c *Config) RequestTokens() int64 {
	if c == nil || c.DefaultRequestTokens <= 0 {
		return DefaultRequestTokensEstimate
	}
	return c.DefaultRequestTokens
}

// ReservePercent returns the effective critical reserve percentage.
func (c *Config) ReservePercent() int64 {
	if c == nil || c.CriticalReservePercent == nil {
		return DefaultCriticalReservePercent
	}
	return *c.CriticalReservePercent
}

// BestEffortCeiling returns the utilization (0-1) above which best-effort and
// custom traffic may no longer consume an allowance.
func (c *Config) BestEffortCeiling() float64 {
	return float64(100-c.ReservePercent()) / 100
}

// Replicas returns the configured data plane replica count (at least 1).
func (c *Config) Replicas() int {
	if c == nil || c.DataplaneReplicas == nil {
		return DefaultDataplaneReplicas
	}
	return *c.DataplaneReplicas
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
	// OverAllowance is set when critical traffic was admitted beyond the
	// local allowance (it is never shed, but this is worth counting).
	OverAllowance bool `json:"over_allowance,omitempty"`
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
