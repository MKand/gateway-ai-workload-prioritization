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
	MinPollInterval            = 15 * time.Second
	DefaultPollTimeout         = 10 * time.Second
	MinSafetyMarginPercent     = 10
	MaxSafetyMarginPercent     = 90
	MinShedThresholdBestEffort = 0.10
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
	if c.OrgId == "" {
		return nil, errors.New("config validation failed: OrgID cannot be empty")
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
	if c.ShedThresholdBestEffort < MinShedThresholdBestEffort || c.ShedThresholdBestEffort > 1.0 {
		c.ShedThresholdBestEffort = DefaultShedThresholdBestEffort
	}

	if c.DefaultProjectLimits == nil {
		c.DefaultProjectLimits = make(map[string]*ModelLimit)
	}
	if c.DefaultOrgLimits == nil {
		c.DefaultOrgLimits = make(map[string]*ModelLimit)
	}
	if c.CustomPolicies == nil {
		c.CustomPolicies = make(map[string]CustomPolicy)
	}
	if err := validateCascadeTargets(c); err != nil {
		return nil, err
	}

	return c, nil
}

// validateCascadeTargets ensures every cascade fallback (model, region) is
// also listed in models/regions. The control plane only polls quota for those,
// so an unlisted fallback would have no quota data and be skipped at runtime.
func validateCascadeTargets(c *Config) error {
	regions := make(map[string]bool, len(c.Regions))
	for _, r := range c.Regions {
		regions[strings.ToLower(strings.TrimSpace(r))] = true
	}
	models := make(map[string]bool, len(c.Models))
	for _, m := range c.Models {
		models[strings.ToLower(strings.TrimSpace(m))] = true
	}

	names := make([]string, 0, len(c.CustomPolicies))
	for name := range c.CustomPolicies {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic error messages

	for _, name := range names {
		for i, step := range c.CustomPolicies[name].Cascade {
			if !regions[strings.ToLower(strings.TrimSpace(step.TargetRegion))] {
				return fmt.Errorf("config validation failed: custom_policies.%s.cascade[%d] region %q is not listed in regions", name, i, step.TargetRegion)
			}
			if !models[strings.ToLower(strings.TrimSpace(step.TargetModel))] {
				return fmt.Errorf("config validation failed: custom_policies.%s.cascade[%d] target_model %q is not listed in models", name, i, step.TargetModel)
			}
		}
	}
	return nil
}
