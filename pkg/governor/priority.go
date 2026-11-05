package governor

import (
	"strings"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
)

// DefaultRetryAfter is the Retry-After duration returned on 429s.
const DefaultRetryAfter = 5 * time.Second

// IsSaturated reports whether a target has no usable allowance left: either
// the headroom after the safety margin is used up, or utilization has reached
// 100%. A nil quota is never saturated (unknown targets fail open).
func IsSaturated(quota *pb.ModelQuota) bool {
	if quota == nil {
		return false
	}
	if quota.UtilizationRpm >= 1 || quota.UtilizationTpm >= 1 {
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

// ParsePriorityName maps a priority name ("critical", "best_effort",
// "custom"; case-insensitive, "-" accepted for "_") to a Priority. It reports
// false for empty or unknown names.
func ParsePriorityName(v string) (Priority, bool) {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(v)), "-", "_") {
	case "critical":
		return Priority_PRIORITY_CRITICAL, true
	case "best_effort", "besteffort":
		return Priority_PRIORITY_BEST_EFFORT, true
	case "custom":
		return Priority_PRIORITY_CUSTOM, true
	default:
		return Priority_PRIORITY_UNSPECIFIED, false
	}
}
