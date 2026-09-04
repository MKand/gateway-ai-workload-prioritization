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
}

// NewCascadeEngine creates a CascadeEngine with default and optional custom cascade chains.
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
		customPolicies: normPolicies,
	}, nil
}

// Evaluate evaluates whether the requested model/region can serve the request or if a fallback is required.
func (ce *CascadeEngine) Evaluate(snapshot *pb.QuotaSnapshot, project, region, model string, priority pb.Priority, policyName string) pb.Decision {
	primaryQuota := getEffectiveQuota(snapshot, project, region, model)
	if primaryQuota == nil {
		return pb.Decision{
			Drop:   false,
			Reason: "primary quota data unavailable, admitting optimistically",
		}
	}

	normPolicyName := strings.ToLower(strings.TrimSpace(policyName))
	isPrimarySaturated := IsSaturated(primaryQuota)

	// 1. CRITICAL: Pass-through (never mutate or drop)
	if priority == pb.Priority_PRIORITY_CRITICAL {
		reason := "priority traffic is not modified. The target status is NOT saturated"
		if isPrimarySaturated {
			reason = "priority traffic is not modified. WARNING: The target status is near saturated"
		}
		return pb.Decision{
			Drop:   false,
			Reason: reason,
		}
	}

	// 2. BEST_EFFORT: Shed if saturated or above threshold; never cascades
	if priority == pb.Priority_PRIORITY_BEST_EFFORT || priority == pb.Priority_PRIORITY_UNSPECIFIED {
		isBestEffortShed := primaryQuota.UtilizationRpm >= DefaultShedThresholdBestEffort || primaryQuota.UtilizationTpm >= DefaultShedThresholdBestEffort

		if isPrimarySaturated || isBestEffortShed {
			return pb.Decision{
				Drop:       true,
				Reason:     fmt.Sprintf("primary model %s is saturated/exceeded threshold (%s); best_effort does not cascade", model, formatUtilization(primaryQuota)),
				RetryAfter: durationpb.New(DefaultRetryAfter),
			}
		}
		return pb.Decision{
			Drop:   false,
			Reason: "best_effort admitted within threshold",
		}
	}

	// 3. CUSTOM: Traverses explicit fallback steps if primary is saturated
	if priority == pb.Priority_PRIORITY_CUSTOM {
		if !isPrimarySaturated {
			return pb.Decision{
				Drop:   false,
				Reason: fmt.Sprintf("custom traffic: primary %s in %s healthy, admitted", model, region),
			}
		}

		policy, exists := ce.customPolicies[normPolicyName]
		if !exists || len(policy.Cascade) == 0 {
			return pb.Decision{
				Drop:       true,
				Reason:     fmt.Sprintf("ERROR: given policy name %s does not exist or has empty cascade; primary model %s is saturated (%s); Treating as best_effort does not cascade", normPolicyName, model, formatUtilization(primaryQuota)),
				RetryAfter: durationpb.New(DefaultRetryAfter),
			}
		}

		for _, cs := range policy.Cascade {
			quota := getEffectiveQuota(snapshot, project, cs.TargetRegion, cs.TargetModel)
			if quota != nil && !IsSaturated(quota) {
				return pb.Decision{
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

		return pb.Decision{
			Drop:       true,
			Reason:     fmt.Sprintf("Primary target and cascade targets defined in policy %s are all saturated; Dropping traffic", normPolicyName),
			RetryAfter: durationpb.New(DefaultRetryAfter),
		}
	}

	return pb.Decision{
		Drop:   false,
		Reason: "primary is healthy, traffic is being forwarded as expected",
	}
}

// getEffectiveQuota returns the project-level quota if present, otherwise falls back to org-level quota.
func getEffectiveQuota(snapshot *pb.QuotaSnapshot, project, region, model string) *pb.ModelQuota {
	if snapshot == nil {
		return nil
	}

	// 1. Try project-specific quota first
	if project != "" {
		if q, err := snapshot.GetProjectQuota(project, region, model); err == nil && q != nil {
			return q
		}
	}

	// 2. Fall back to organization pooled quota
	if q, err := snapshot.GetOrgQuota(region, model); err == nil && q != nil {
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
