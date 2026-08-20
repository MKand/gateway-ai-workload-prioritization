package governor

import (
	"fmt"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	// DefaultShedThresholdBestEffort defines the utilization threshold (70%) above which
	// best-effort traffic is shed with HTTP 429.
	DefaultShedThresholdBestEffort = 0.70

	// DefaultSaturationThreshold defines the utilization threshold (95%) above which
	// critical traffic must trigger a fallback cascade.
	DefaultSaturationThreshold = 0.95

	// DefaultRetryAfter is the Retry-After duration returned on 429s.
	DefaultRetryAfter = 5 * time.Second
)

// DefaultPriorityPolicy implements the PriorityPolicy interface.
type DefaultPriorityPolicy struct {
	ShedThreshold float64
}

// NewDefaultPriorityPolicy creates a PriorityPolicy with a configurable shedding threshold.
func NewDefaultPriorityPolicy(shedThreshold float64) *DefaultPriorityPolicy {
	if shedThreshold <= 0 || shedThreshold > 1.0 {
		shedThreshold = DefaultShedThresholdBestEffort
	}
	return &DefaultPriorityPolicy{ShedThreshold: shedThreshold}
}

// Name returns the policy identifier.
func (p *DefaultPriorityPolicy) Name() string {
	return "default_priority_policy"
}

// Evaluate evaluates a ModelQuota assuming best-effort priority by default.
func (p *DefaultPriorityPolicy) Evaluate(quota *pb.ModelQuota) pb.Decision {
	return EvaluatePriority(pb.Priority_PRIORITY_BEST_EFFORT, quota, p.ShedThreshold)
}

// EvaluatePriority evaluates the admission of a request based on its priority tier and real-time quota state.
func EvaluatePriority(priority pb.Priority, quota *pb.ModelQuota, shedThreshold float64) pb.Decision {
	if shedThreshold <= 0 || shedThreshold > 1.0 {
		shedThreshold = DefaultShedThresholdBestEffort
	}

	if quota == nil {
		// If quota data is unavailable, fail open (allow forwarding) to avoid complete outages
		return pb.Decision{
			Drop:   false,
			Reason: "quota data unavailable, admitting optimistically",
		}
	}

	// 1. BEST EFFORT TRAFFIC: Early shedding at > 70% threshold
	if priority == pb.Priority_PRIORITY_BEST_EFFORT || priority == pb.Priority_PRIORITY_UNSPECIFIED {
		if quota.UtilizationRpm >= shedThreshold || quota.UtilizationTpm >= shedThreshold {
			return pb.Decision{
				Drop: true,
				Reason: fmt.Sprintf(
					"best_effort shed: utilization (RPM: %.1f%%, TPM: %.1f%%) exceeded threshold %.1f%%",
					quota.UtilizationRpm*100,
					quota.UtilizationTpm*100,
					shedThreshold*100,
				),
				RetryAfter: durationpb.New(DefaultRetryAfter),
			}
		}

		// Also shed if headroom is zero or negative when limits are defined
		if (quota.MaxRpm > 0 && quota.HeadroomRpm <= 0) || (quota.MaxTpm > 0 && quota.HeadroomTpm <= 0) {
			return pb.Decision{
				Drop:       true,
				Reason:     "best_effort shed: no usable headroom remaining",
				RetryAfter: durationpb.New(DefaultRetryAfter),
			}
		}

		return pb.Decision{
			Drop:   false,
			Reason: "best_effort admitted within quota limits",
		}
	}

	// 2. CRITICAL TRAFFIC: Protected up to saturation threshold (95%)
	if priority == pb.Priority_PRIORITY_CRITICAL {
		if IsSaturated(quota) {
			// Saturated: needs fallback cascade
			return pb.Decision{
				Drop: false,
				Reason: fmt.Sprintf(
					"critical capacity saturated (RPM: %.1f%%, TPM: %.1f%%): fallback cascade required",
					quota.UtilizationRpm*100,
					quota.UtilizationTpm*100,
				),
			}
		}

		return pb.Decision{
			Drop:   false,
			Reason: "critical admitted with guaranteed headroom",
		}
	}

	// 3. CUSTOM TRAFFIC: Admitted by default (evaluated by custom cascade DAGs)
	return pb.Decision{
		Drop:   false,
		Reason: "custom priority admitted",
	}
}

// IsSaturated returns true if the model quota has reached or exceeded 95% utilization.
func IsSaturated(quota *pb.ModelQuota) bool {
	if quota == nil {
		return false
	}
	if quota.UtilizationRpm >= DefaultSaturationThreshold || quota.UtilizationTpm >= DefaultSaturationThreshold {
		return true
	}
	if quota.MaxRpm > 0 && quota.HeadroomRpm <= 0 {
		return true
	}
	if quota.MaxTpm > 0 && quota.HeadroomTpm <= 0 {
		return true
	}
	return false
}
