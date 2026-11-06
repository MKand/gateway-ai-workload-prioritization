package governor

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	MinPollInterval        = 15 * time.Second
	DefaultPollTimeout     = 10 * time.Second
	MinSafetyMarginPercent = 10
	MaxSafetyMarginPercent = 90

	// DefaultCriticalReservePercent is the share of each allowance reserved
	// for critical traffic when critical_reserve_percent is unset.
	DefaultCriticalReservePercent int64 = 30
	MinCriticalReservePercent     int64 = 0
	MaxCriticalReservePercent     int64 = 90

	// DefaultDataplaneReplicas is used when dataplane_replicas is unset.
	DefaultDataplaneReplicas = 1

	// DefaultRequestTokensEstimate is charged for requests without size hints
	// when default_request_tokens is unset.
	DefaultRequestTokensEstimate int64 = 1000
)

func LoadConfig(path string) (*Config, error) {
	c := &Config{}
	f, err := os.ReadFile(path)

	if err != nil {
		return nil, fmt.Errorf("unable to read config file %s: %w", path, err)
	}
	if err := yaml.Unmarshal(f, c); err != nil {
		return nil, fmt.Errorf("unable to unmarshal config file %s: %w", path, err)
	}

	if len(c.ProjectIDs) == 0 {
		return nil, errors.New("config validation failed: ProjectIds cannot be empty")
	}
	if len(c.Regions) == 0 {
		return nil, errors.New("config validation failed: regions cannot be empty")
	}
	if len(c.Models) == 0 {
		return nil, errors.New("config validation failed: models cannot be empty")
	}

	if c.PollInterval < MinPollInterval {
		c.PollInterval = MinPollInterval
	}
	if c.PollTimeout <= 0 || c.PollTimeout > c.PollInterval {
		c.PollTimeout = DefaultPollTimeout
		if c.PollTimeout > c.PollInterval {
			c.PollTimeout = c.PollInterval
		}
	}
	if c.SafetyMarginPercent < MinSafetyMarginPercent || c.SafetyMarginPercent > MaxSafetyMarginPercent {
		c.SafetyMarginPercent = MinSafetyMarginPercent
	}

	if c.DefaultProjectLimits == nil {
		c.DefaultProjectLimits = make(map[string]*ModelLimit)
	}
	if c.CustomPolicies == nil {
		c.CustomPolicies = make(map[string]CustomPolicy)
	}
	if err := validateAdmissionSettings(c); err != nil {
		return nil, err
	}
	if err := normalizeAllowances(c); err != nil {
		return nil, err
	}
	if err := validateCascadeTargets(c); err != nil {
		return nil, err
	}

	return c, nil
}

// validateAdmissionSettings checks the priority-admission knobs. Unlike the
// polling knobs above, invalid values are rejected rather than clamped: a
// silently changed reserve would quietly change who gets shed.
func validateAdmissionSettings(c *Config) error {
	if c.CriticalReservePercent != nil {
		if r := *c.CriticalReservePercent; r < MinCriticalReservePercent || r > MaxCriticalReservePercent {
			return fmt.Errorf("config validation failed: critical_reserve_percent %d must be between %d and %d", r, MinCriticalReservePercent, MaxCriticalReservePercent)
		}
	}

	if c.DefaultRequestTokens < 0 {
		return fmt.Errorf("config validation failed: default_request_tokens %d cannot be negative", c.DefaultRequestTokens)
	}

	if c.DataplaneReplicas != nil && *c.DataplaneReplicas < 1 {
		return fmt.Errorf("config validation failed: dataplane_replicas %d must be at least 1", *c.DataplaneReplicas)
	}
	return nil
}

// normalizeAllowances lower-cases allowance keys and checks that each one is a
// "location/model" pair drawn from regions and models with a positive limit.
func normalizeAllowances(c *Config) error {
	if len(c.Allowances) == 0 {
		return errors.New("config validation failed: allowances cannot be empty")
	}
	regions := normalizedSet(c.Regions)
	models := normalizedSet(c.Models)

	keys := make([]string, 0, len(c.Allowances))
	for k := range c.Allowances {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic error messages

	out := make(map[string]*ModelLimit, len(c.Allowances))
	for _, raw := range keys {
		limit := c.Allowances[raw]
		key := strings.ToLower(strings.TrimSpace(raw))
		location, model, ok := strings.Cut(key, "/")
		if !ok || location == "" || model == "" || strings.Contains(model, "/") {
			return fmt.Errorf("config validation failed: allowances key %q must be \"location/model\"", raw)
		}
		if !regions[location] {
			return fmt.Errorf("config validation failed: allowances key %q: location %q is not listed in regions", raw, location)
		}
		if !models[model] {
			return fmt.Errorf("config validation failed: allowances key %q: model %q is not listed in models", raw, model)
		}
		if limit == nil || limit.MaxRpm < 0 || limit.MaxTpm < 0 || (limit.MaxRpm == 0 && limit.MaxTpm == 0) {
			return fmt.Errorf("config validation failed: allowances key %q needs max_rpm and/or max_tpm > 0 (and neither negative)", raw)
		}
		if _, dup := out[key]; dup {
			return fmt.Errorf("config validation failed: allowances key %q is duplicated (keys are case-insensitive)", raw)
		}
		out[key] = limit
	}
	c.Allowances = out
	return nil
}

func normalizedSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[strings.ToLower(strings.TrimSpace(v))] = true
	}
	return set
}

// validateCascadeTargets ensures every cascade fallback (model, region) is
// also listed in models/regions and has an allowance. The control plane only
// polls quota for listed targets, and the governor can only admit traffic
// against an allowance it knows.
func validateCascadeTargets(c *Config) error {
	regions := normalizedSet(c.Regions)
	models := normalizedSet(c.Models)

	names := make([]string, 0, len(c.CustomPolicies))
	for name := range c.CustomPolicies {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic error messages

	for _, name := range names {
		for i, step := range c.CustomPolicies[name].Cascade {
			region := strings.ToLower(strings.TrimSpace(step.TargetRegion))
			model := strings.ToLower(strings.TrimSpace(step.TargetModel))
			if !regions[region] {
				return fmt.Errorf("config validation failed: custom_policies.%s.cascade[%d] region %q is not listed in regions", name, i, step.TargetRegion)
			}
			if !models[model] {
				return fmt.Errorf("config validation failed: custom_policies.%s.cascade[%d] target_model %q is not listed in models", name, i, step.TargetModel)
			}
			if _, ok := c.Allowances[region+"/"+model]; !ok {
				return fmt.Errorf("config validation failed: custom_policies.%s.cascade[%d] target %q has no entry in allowances", name, i, region+"/"+model)
			}
		}
	}
	return nil
}
