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
	// limiter enforces the configured allowances locally. nil means decisions
	// use the snapshot only.
	limiter *Limiter
}

// Request describes one request to decide on.
type Request struct {
	Project  string
	Region   string // location, e.g. "us-central1" or "global"
	Model    string
	Priority Priority
	Policy   string // X-Governor-Policy, used by custom traffic
	Tokens   int64  // estimated input tokens
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
	if ce.limiter, err = NewLimiter(cfg, nil); err != nil {
		return nil, err
	}
	return ce, nil
}

// Limiter returns the engine's local limiter (nil if none).
func (ce *CascadeEngine) Limiter() *Limiter {
	return ce.limiter
}

// WithLimiter replaces the engine's local limiter (for example one with a
// fake clock in tests) and returns the engine.
func (ce *CascadeEngine) WithLimiter(l *Limiter) *CascadeEngine {
	ce.limiter = l
	return ce
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

// Evaluate decides on a request without a token estimate. It is kept for
// callers and tests that only care about request counts; see Decide.
func (ce *CascadeEngine) Evaluate(snapshot *pb.QuotaSnapshot, project, region, model string, priority Priority, policyName string) Decision {
	return ce.Decide(snapshot, Request{Project: project, Region: region, Model: model, Priority: priority, Policy: policyName})
}

// Decide decides whether a request is forwarded, rerouted or shed.
//
// A target has room for best-effort and custom traffic only if both the
// snapshot (when it has data for the target) and the local limiter (when an
// allowance is configured) say it is below the critical reserve. Critical
// traffic is never shed. Targets that neither source knows are admitted
// optimistically (fail open).
func (ce *CascadeEngine) Decide(snapshot *pb.QuotaSnapshot, r Request) Decision {
	primaryQuota := getEffectiveQuota(snapshot, r.Project, r.Region, r.Model)
	if primaryQuota == nil && !ce.limiter.Known(r.Region, r.Model) {
		return Decision{
			Drop:   false,
			Reason: "primary quota data unavailable, admitting optimistically",
		}
	}

	switch r.Priority {
	// 1. CRITICAL: never mutated or dropped. It may use the reserve, and
	// beyond the allowance it is still admitted (and counted as debt).
	case Priority_PRIORITY_CRITICAL:
		within, _ := ce.limiter.AdmitCritical(r.Region, r.Model, r.Tokens)
		reason := "priority traffic is not modified. The target status is NOT saturated"
		if !within || IsSaturated(primaryQuota) {
			reason = "priority traffic is not modified. WARNING: the allowance for the target is used up"
		}
		return Decision{Drop: false, Reason: reason, OverAllowance: !within}

	// 3. CUSTOM: like best effort, custom traffic may not use the critical
	// reserve. Once the primary is full, traverse the fallback steps.
	case Priority_PRIORITY_CUSTOM:
		ok, why := ce.tryTarget(snapshot, r.Project, r.Region, r.Model, r.Tokens)
		if ok {
			return Decision{
				Drop:   false,
				Reason: fmt.Sprintf("custom traffic: primary %s in %s healthy, admitted", r.Model, r.Region),
			}
		}

		normPolicyName := strings.ToLower(strings.TrimSpace(r.Policy))
		policy, exists := ce.customPolicies[normPolicyName]
		if !exists || len(policy.Cascade) == 0 {
			return Decision{
				Drop:       true,
				Reason:     fmt.Sprintf("ERROR: given policy name %s does not exist or has empty cascade; %s; Treating as best_effort does not cascade", normPolicyName, why),
				RetryAfter: durationpb.New(DefaultRetryAfter),
			}
		}

		for _, cs := range policy.Cascade {
			if !ce.knows(snapshot, r.Project, cs.TargetRegion, cs.TargetModel) {
				continue
			}
			if ok, _ := ce.tryTarget(snapshot, r.Project, cs.TargetRegion, cs.TargetModel, r.Tokens); ok {
				return Decision{
					Drop:          false,
					ReplaceModel:  cs.TargetModel,
					ReplaceRegion: cs.TargetRegion,
					Reason: fmt.Sprintf(
						"combined failover: %s in %s saturated -> rerouted to %s in %s",
						r.Model, r.Region, cs.TargetModel, cs.TargetRegion,
					),
				}
			}
		}

		return Decision{
			Drop:       true,
			Reason:     fmt.Sprintf("Primary target and cascade targets defined in policy %s are all saturated; Dropping traffic", normPolicyName),
			RetryAfter: durationpb.New(DefaultRetryAfter),
		}

	// 2. BEST_EFFORT (and unlabelled traffic, which is always treated as best
	// effort): shed once the target reaches the critical reserve; never cascades.
	default:
		if ok, why := ce.tryTarget(snapshot, r.Project, r.Region, r.Model, r.Tokens); !ok {
			return Decision{
				Drop:       true,
				Reason:     fmt.Sprintf("%s; best_effort does not cascade", why),
				RetryAfter: durationpb.New(DefaultRetryAfter),
			}
		}
		return Decision{
			Drop:   false,
			Reason: "best_effort admitted within threshold",
		}
	}
}

// tryTarget checks whether a target has room for best-effort or custom
// traffic and, if so, reserves the request in the local limiter. The snapshot
// is checked first so a full target never consumes local tokens.
func (ce *CascadeEngine) tryTarget(snapshot *pb.QuotaSnapshot, project, region, model string, tokens int64) (bool, string) {
	if q := getEffectiveQuota(snapshot, project, region, model); q != nil && (IsSaturated(q) || ce.aboveCeiling(q)) {
		return false, fmt.Sprintf("model %s in %s is saturated/exceeded threshold (%s)", model, region, formatUtilization(q))
	}
	if ok, known := ce.limiter.TryAdmit(region, model, tokens); known && !ok {
		return false, fmt.Sprintf("model %s in %s reached the critical reserve of its local allowance", model, region)
	}
	return true, ""
}

// knows reports whether the snapshot or the limiter has data for a target.
func (ce *CascadeEngine) knows(snapshot *pb.QuotaSnapshot, project, region, model string) bool {
	return getEffectiveQuota(snapshot, project, region, model) != nil || ce.limiter.Known(region, model)
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
