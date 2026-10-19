package dataplane

import (
	"strconv"
	"strings"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/router"
)

// Request headers read by the governor.
const (
	HeaderPriority  = "x-request-priority"
	HeaderPolicy    = "x-governor-policy"
	HeaderPath      = ":path"
	HeaderAuthority = ":authority"
)

// Headers written by the governor. Any client-supplied values for these are
// removed or overwritten so upstreams can trust them.
const (
	HeaderDecision     = "x-governor-decision"
	HeaderReason       = "x-governor-reason"
	HeaderTargetModel  = "x-governor-target-model"
	HeaderTargetRegion = "x-governor-target-region"
	HeaderOriginalPath = "x-governor-original-path"
	HeaderRetryAfter   = "retry-after"
)

// Values of the x-governor-decision header.
const (
	DecisionForward = "forward" // admitted unchanged
	DecisionReroute = "reroute" // :path/:authority rewritten to the fallback model/region
	DecisionShed    = "shed"    // rejected with HTTP 429
	DecisionBypass  = "bypass"  // not a recognised Vertex AI request; untouched
)

// governorHeaders lists every header the governor owns.
var governorHeaders = []string{HeaderDecision, HeaderReason, HeaderTargetModel, HeaderTargetRegion, HeaderOriginalPath}

// Result is the transport-agnostic outcome for one request. The ext_proc layer
// translates it into Envoy header mutations or an immediate 429.
type Result struct {
	Decision string
	// Shed is true when the request must be rejected with HTTP 429.
	Shed bool
	// RetryAfterSeconds is set when Shed is true.
	RetryAfterSeconds int64
	// SetHeaders are headers to add or overwrite on the upstream request
	// (or on the 429 response when Shed is true).
	SetHeaders map[string]string
	// RemoveHeaders are client-supplied governor headers to strip.
	RemoveHeaders []string
	// RouteChanged is true when :path or :authority was rewritten, so Envoy
	// must recompute the route.
	RouteChanged bool
}

// Evaluator decides what to do with a request given the latest quota snapshot.
type Evaluator struct {
	cache  *SnapshotCache
	engine *governor.CascadeEngine
}

// NewEvaluator builds an Evaluator. When the cascade engine selects a fallback,
// the request's :path and :authority are rewritten so Envoy proxies it to the
// fallback model/region.
func NewEvaluator(cache *SnapshotCache, engine *governor.CascadeEngine) *Evaluator {
	return &Evaluator{cache: cache, engine: engine}
}

// ParsePriority maps an X-Request-Priority header value to a Priority. Unknown
// or missing values map to PRIORITY_UNSPECIFIED, which is treated as best effort.
func ParsePriority(v string) governor.Priority {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "critical":
		return governor.Priority_PRIORITY_CRITICAL
	case "best_effort", "best-effort", "besteffort":
		return governor.Priority_PRIORITY_BEST_EFFORT
	case "custom":
		return governor.Priority_PRIORITY_CUSTOM
	default:
		return governor.Priority_PRIORITY_UNSPECIFIED
	}
}

// Evaluate computes the Result for a request. headers must have lower-case keys.
func (e *Evaluator) Evaluate(headers map[string]string) Result {
	res := Result{
		SetHeaders:    map[string]string{},
		RemoveHeaders: append([]string(nil), governorHeaders...),
	}

	info, err := router.Parse(headers[HeaderPath], headers[HeaderAuthority])
	if err != nil {
		res.Decision = DecisionBypass
		res.SetHeaders[HeaderDecision] = DecisionBypass
		res.SetHeaders[HeaderReason] = "not a vertex ai model request: " + err.Error()
		res.RemoveHeaders = without(res.RemoveHeaders, HeaderDecision, HeaderReason)
		return res
	}

	priority := ParsePriority(headers[HeaderPriority])
	d := e.engine.Evaluate(e.cache.Get(), info.ProjectID, info.Region, info.Model, priority, headers[HeaderPolicy])

	res.SetHeaders[HeaderReason] = d.Reason

	switch {
	case d.Drop:
		res.Decision = DecisionShed
		res.Shed = true
		res.RetryAfterSeconds = int64(governor.DefaultRetryAfter.Seconds())
		if d.RetryAfter != nil {
			res.RetryAfterSeconds = d.RetryAfter.GetSeconds()
		}
		res.SetHeaders[HeaderRetryAfter] = strconv.FormatInt(res.RetryAfterSeconds, 10)

	case d.ReplaceModel != "" || d.ReplaceRegion != "":
		newPath, newHost := router.Mutate(info, d.ReplaceModel, d.ReplaceRegion)
		res.Decision = DecisionReroute
		res.RouteChanged = true
		res.SetHeaders[HeaderPath] = newPath
		if newHost != "" && newHost != headers[HeaderAuthority] {
			res.SetHeaders[HeaderAuthority] = newHost
		}
		res.SetHeaders[HeaderOriginalPath] = headers[HeaderPath]
		res.SetHeaders[HeaderTargetModel] = firstNonEmpty(d.ReplaceModel, info.Model)
		res.SetHeaders[HeaderTargetRegion] = firstNonEmpty(d.ReplaceRegion, info.Region)

	default:
		res.Decision = DecisionForward
	}

	res.SetHeaders[HeaderDecision] = res.Decision
	for k := range res.SetHeaders {
		res.RemoveHeaders = without(res.RemoveHeaders, k)
	}
	return res
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func without(list []string, drop ...string) []string {
	out := list[:0]
	for _, v := range list {
		keep := true
		for _, d := range drop {
			if v == d {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, v)
		}
	}
	return out
}
