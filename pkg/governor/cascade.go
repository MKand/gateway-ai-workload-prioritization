package governor

import (
	"errors"
	"fmt"
	"strings"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// CascadeEngine manages model downgrade and regional failover evaluation.
type CascadeEngine struct {
	customPolicies map[string]CustomPolicy
	// bestEffortCeiling is the utilization (0-1) at which best-effort traffic
	// is shed; everything above it is reserved for critical traffic.
	bestEffortCeiling float64
}

// NewCascadeEngineFromConfig builds a CascadeEngine from a loaded Config, so
// the critical reserve comes from configuration.
func NewCascadeEngineFromConfig(cfg *Config) (*CascadeEngine, error) {
	if cfg == nil {
		return nil, errors.New("config cannot be nil")
	}
	ce, err := NewCascadeEngine(cfg.CustomPolicies)
	if err != nil {
		return nil, err
	}
	ce.bestEffortCeiling = cfg.BestEffortCeiling()
	return ce, nil
}

// NewCascadeEngine creates a CascadeEngine with custom cascade chains and the
// default critical reserve. Prefer
// NewCascadeEngineFromConfig in binaries.
func NewCascadeEngine(customPolicies map[string]CustomPolicy) (*CascadeEngine, error) {
	normPolicies := make(map[string]CustomPolicy)

	for name, policy := range customPolicies {
		normName := strings.ToLower(strings.TrimSpace(name))
		if normName == "" {
			return nil, errors.New("policy name cannot be empty")
		}
		if len(policy.Cascade) == 0 {
			return nil, fmt.Errorf("policy %q must define at least one cascade step", name)
		}

		normCascade := make([]CascadeStep, 0, len(policy.Cascade))

		for _, p := range policy.Cascade {
			normTargetModel := strings.TrimSpace(p.TargetModel)

			if normTargetModel == "" {
				return nil, errors.New("TargetModel in CascadeStep cannot be empty")
			}
			normTargetRegion := strings.TrimSpace(p.TargetRegion)

			if normTargetRegion == "" {
				return nil, errors.New("TargetRegion in CascadeStep cannot be empty")
			}
			normCascade = append(normCascade, CascadeStep{
				TargetModel:  normTargetModel,
				TargetRegion: normTargetRegion,
			})
		}
		normPolicies[normName] = CustomPolicy{
			Cascade: normCascade,
		}

	}
	return &CascadeEngine{
		customPolicies:    normPolicies,
		bestEffortCeiling: float64(100-DefaultCriticalReservePercent) / 100,
	}, nil
}

// Evaluate evaluates whether the requested model/region can serve the request or if a fallback is required.
func (ce *CascadeEngine) Evaluate(snapshot *pb.QuotaSnapshot, project, region, model string, priority Priority, policyName string) Decision {
	primaryQuota := getEffectiveQuota(snapshot, project, region, model)
	if primaryQuota == nil {
		return Decision{
			Drop:   false,
			Reason: "primary quota data unavailable, admitting optimistically",
		}
	}

	normPolicyName := strings.ToLower(strings.TrimSpace(policyName))
	isPrimarySaturated := IsSaturated(primaryQuota)
	isPrimaryAboveCeiling := isPrimarySaturated || ce.aboveCeiling(primaryQuota)

	// 1. CRITICAL: Pass-through (never mutate or drop)
	if priority == Priority_PRIORITY_CRITICAL {
		reason := "priority traffic is not modified. The target status is NOT saturated"
		if isPrimarySaturated {
			reason = "priority traffic is not modified. WARNING: The target status is near saturated"
		}
		return Decision{
			Drop:   false,
			Reason: reason,
		}
	}

	// 2. BEST_EFFORT (and unlabelled traffic, which is always treated as best
	// effort): Shed if saturated or above the ceiling (the critical reserve
	// starts there); never cascades.
	if priority == Priority_PRIORITY_BEST_EFFORT || priority == Priority_PRIORITY_UNSPECIFIED {
		if isPrimaryAboveCeiling {
			return Decision{
				Drop:       true,
				Reason:     fmt.Sprintf("primary model %s is saturated/exceeded threshold (%s); best_effort does not cascade", model, formatUtilization(primaryQuota)),
				RetryAfter: durationpb.New(DefaultRetryAfter),
			}
		}
		return Decision{
			Drop:   false,
			Reason: "best_effort admitted within threshold",
		}
	}

	// 3. CUSTOM: like best effort, custom traffic may not use the critical
	// reserve. Once the primary reaches the ceiling, traverse the fallback steps.
	if priority == Priority_PRIORITY_CUSTOM {
		if !isPrimaryAboveCeiling {
			return Decision{
				Drop:   false,
				Reason: fmt.Sprintf("custom traffic: primary %s in %s healthy, admitted", model, region),
			}
		}

		policy, exists := ce.customPolicies[normPolicyName]
		if !exists || len(policy.Cascade) == 0 {
			return Decision{
				Drop:       true,
				Reason:     fmt.Sprintf("ERROR: given policy name %s does not exist or has empty cascade; primary model %s is saturated (%s); Treating as best_effort does not cascade", normPolicyName, model, formatUtilization(primaryQuota)),
				RetryAfter: durationpb.New(DefaultRetryAfter),
			}
		}

		for _, cs := range policy.Cascade {
			quota := getEffectiveQuota(snapshot, project, cs.TargetRegion, cs.TargetModel)
			if quota != nil && !IsSaturated(quota) && !ce.aboveCeiling(quota) {
				return Decision{
					Drop:          false,
					ReplaceModel:  cs.TargetModel,
					ReplaceRegion: cs.TargetRegion,
					Reason: fmt.Sprintf(
						"combined failover: %s in %s saturated -> rerouted to %s in %s",
						model, region, cs.TargetModel, cs.TargetRegion,
					),
				}
			}
		}

		return Decision{
			Drop:       true,
			Reason:     fmt.Sprintf("Primary target and cascade targets defined in policy %s are all saturated; Dropping traffic", normPolicyName),
			RetryAfter: durationpb.New(DefaultRetryAfter),
		}
	}

	return Decision{
		Drop:   false,
		Reason: "primary is healthy, traffic is being forwarded as expected",
	}
}

// aboveCeiling reports whether a target's RPM or TPM utilization has reached
// the best-effort ceiling, i.e. the remaining allowance is the critical reserve.
func (ce *CascadeEngine) aboveCeiling(q *pb.ModelQuota) bool {
	return q != nil && (q.UtilizationRpm >= ce.bestEffortCeiling || q.UtilizationTpm >= ce.bestEffortCeiling)
}

// getEffectiveQuota returns the project-level quota if present, otherwise falls back to org-level quota.
func getEffectiveQuota(snapshot *pb.QuotaSnapshot, project, region, model string) *pb.ModelQuota {
	if snapshot == nil {
		return nil
	}

	// 1. Try project-specific quota first
	if project != "" {
		if q, err := GetProjectQuota(snapshot, project, region, model); err == nil && q != nil {
			return q
		}
	}

	// 2. Fall back to organization pooled quota
	if q, err := GetOrgQuota(snapshot, region, model); err == nil && q != nil {
		return q
	}

	return nil
}

func formatUtilization(q *pb.ModelQuota) string {
	if q == nil {
		return "unknown quota"
	}
	return fmt.Sprintf("RPM: %.1f%%, TPM: %.1f%%", q.UtilizationRpm*100, q.UtilizationTpm*100)
}
