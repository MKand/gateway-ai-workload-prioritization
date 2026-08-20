package governor

import (
	"fmt"
	"strings"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// DefaultModelCascades maps standard model families to their downstream fallback candidates.
var DefaultModelCascades = map[string][]string{
	"gemini-1.5-pro":      {"gemini-1.5-flash", "gemini-1.5-flash-8b", "gemini-1.0-pro"},
	"gemini-2.0-flash":    {"gemini-1.5-flash", "gemini-1.5-flash-8b"},
	"gemini-1.5-flash":    {"gemini-1.5-flash-8b"},
	"gemini-1.5-flash-8b": {},
}

// DefaultRegionCascades maps regions to geographically proximate fallback regions.
var DefaultRegionCascades = map[string][]string{
	"us-central1":     {"us-east4", "us-west1", "us-east1"},
	"us-east4":        {"us-central1", "us-east1", "us-west1"},
	"us-west1":        {"us-central1", "us-east4", "us-west4"},
	"europe-west4":    {"europe-west1", "europe-west3", "us-central1"},
	"asia-northeast1": {"asia-southeast1", "asia-east1", "us-central1"},
}

// CascadeEngine manages model downgrade and regional failover evaluation.
type CascadeEngine struct {
	modelCascades  map[string][]string
	regionCascades map[string][]string
}

// NewCascadeEngine creates a CascadeEngine with default and optional custom cascade chains.
func NewCascadeEngine(customModelCascades map[string][]string, customRegionCascades map[string][]string) *CascadeEngine {
	models := make(map[string][]string)
	for k, v := range DefaultModelCascades {
		models[strings.ToLower(k)] = v
	}
	for k, v := range customModelCascades {
		models[strings.ToLower(k)] = v
	}

	regions := make(map[string][]string)
	for k, v := range DefaultRegionCascades {
		regions[strings.ToLower(k)] = v
	}
	for k, v := range customRegionCascades {
		regions[strings.ToLower(k)] = v
	}

	return &CascadeEngine{
		modelCascades:  models,
		regionCascades: regions,
	}
}

// Evaluate evaluates whether the requested model/region can serve the request or if a fallback is required.
func (ce *CascadeEngine) Evaluate(snapshot *pb.QuotaSnapshot, project, region, model string, priority pb.Priority) pb.Decision {
	primaryQuota := getEffectiveQuota(snapshot, project, region, model)

	// If primary target is healthy and not saturated, forward normally
	if !IsSaturated(primaryQuota) {
		return EvaluatePriority(priority, primaryQuota, DefaultShedThresholdBestEffort)
	}

	// Best-effort traffic is never permitted to cascade down to lighter models
	if priority == pb.Priority_PRIORITY_BEST_EFFORT || priority == pb.Priority_PRIORITY_UNSPECIFIED {
		return pb.Decision{
			Drop:       true,
			Reason:     fmt.Sprintf("primary model %s is saturated (%s); best_effort does not cascade", model, formatUtilization(primaryQuota)),
			RetryAfter: durationpb.New(DefaultRetryAfter),
		}
	}

	// CRITICAL & CUSTOM TRAFFIC: Traverse fallback cascade DAG
	// Phase 1: Try fallback models in the SAME region
	candidateModels := ce.getModelCandidates(model)
	for _, candModel := range candidateModels {
		candQuota := getEffectiveQuota(snapshot, project, region, candModel)
		if isCandidateEligible(candQuota) {
			return pb.Decision{
				Drop:         false,
				ReplaceModel: candModel,
				Reason: fmt.Sprintf(
					"model fallback: primary %s saturated in %s -> downgraded to %s",
					model, region, candModel,
				),
			}
		}
	}

	// Phase 2: Try fallback REGIONS for the requested model (and then fallback models)
	candidateRegions := ce.getRegionCandidates(region)
	for _, candRegion := range candidateRegions {
		// 2a: Try original model in secondary region
		candQuota := getEffectiveQuota(snapshot, project, candRegion, model)
		if isCandidateEligible(candQuota) {
			return pb.Decision{
				Drop:          false,
				ReplaceRegion: candRegion,
				Reason: fmt.Sprintf(
					"regional failover: %s saturated in %s -> rerouted to %s",
					model, region, candRegion,
				),
			}
		}

		// 2b: Try fallback models in secondary region
		for _, candModel := range candidateModels {
			candModelQuota := getEffectiveQuota(snapshot, project, candRegion, candModel)
			if isCandidateEligible(candModelQuota) {
				return pb.Decision{
					Drop:          false,
					ReplaceModel:  candModel,
					ReplaceRegion: candRegion,
					Reason: fmt.Sprintf(
						"combined failover: %s in %s saturated -> rerouted to %s in %s",
						model, region, candModel, candRegion,
					),
				}
			}
		}
	}

	// Phase 3: All fallback candidates exhausted -> drop as last resort
	return pb.Decision{
		Drop: true,
		Reason: fmt.Sprintf(
			"critical drop: all fallback models (%v) and regions (%v) exhausted",
			candidateModels, candidateRegions,
		),
		RetryAfter: durationpb.New(DefaultRetryAfter),
	}
}

// isCandidateEligible returns true only if the candidate has an existing quota entry with available capacity.
func isCandidateEligible(quota *pb.ModelQuota) bool {
	if quota == nil {
		return false
	}
	return !IsSaturated(quota)
}

func (ce *CascadeEngine) getModelCandidates(model string) []string {
	if candidates, ok := ce.modelCascades[strings.ToLower(model)]; ok {
		return candidates
	}
	return nil
}

func (ce *CascadeEngine) getRegionCandidates(region string) []string {
	if candidates, ok := ce.regionCascades[strings.ToLower(region)]; ok {
		return candidates
	}
	return nil
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
