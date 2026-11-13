package dataplane

import (
	"strconv"
	"strings"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/router"
)

// Request headers read by the governor.
const (
	HeaderPriority = "x-request-priority"
	HeaderPolicy   = "x-governor-policy"
	HeaderPath     = ":path"
	// HeaderPromptTokens lets a client state its input-token count.
	HeaderPromptTokens  = "x-prompt-tokens"
	HeaderContentLength = "content-length"
	HeaderAuthority     = ":authority"
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

	// The fields below describe the request for metrics and logs. They are
	// empty for bypassed requests.

	// Priority is the effective priority name ("critical", "best_effort",
	// "custom"); unlabelled traffic is reported as "best_effort".
	Priority string
	// Target is the "location/model" the request is finally sent to (after
	// any reroute), lower case.
	Target string
	// Tokens is the estimated input-token count.
	Tokens int64
	// OverAllowance is true for critical traffic admitted beyond the allowance.
	OverAllowance bool
}

// priorityLabel returns the metric/log name of a priority. Unlabelled traffic
// is always treated as best effort, so it is reported as such.
func priorityLabel(p governor.Priority) string {
	switch p {
	case governor.Priority_PRIORITY_CRITICAL:
		return "critical"
	case governor.Priority_PRIORITY_CUSTOM:
		return "custom"
	default:
		return "best_effort"
	}
}

// Evaluator decides what to do with a request given the latest quota snapshot.
type Evaluator struct {
	cache  *SnapshotCache
	engine *governor.CascadeEngine
	// defaultTokens is charged for requests without any size hint.
	defaultTokens int64
}

// bytesPerToken approximates how many request-body bytes make up one input
// token. Gemini averages about 4 characters per token; JSON framing makes the
// estimate slightly pessimistic, which is the safe direction.
const bytesPerToken = 4

// maxTokenEstimate caps any single estimate so a bogus header cannot drain or
// overflow a bucket.
const maxTokenEstimate int64 = 10_000_000

// WithDefaultTokens sets the estimate for requests without Content-Length or
// X-Prompt-Tokens, and returns the Evaluator. Values <= 0 are ignored.
func (e *Evaluator) WithDefaultTokens(n int64) *Evaluator {
	if n > 0 {
		e.defaultTokens = n
	}
	return e
}

// EstimateTokens estimates a request's input tokens from its headers: the
// larger of X-Prompt-Tokens and Content-Length/4, so a client cannot
// understate a large body. Without either hint it returns defaultTokens.
// headers must have lower-case keys.
func EstimateTokens(headers map[string]string, defaultTokens int64) int64 {
	var est int64
	if v, err := strconv.ParseInt(strings.TrimSpace(headers[HeaderPromptTokens]), 10, 64); err == nil && v > 0 {
		est = v
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(headers[HeaderContentLength]), 10, 64); err == nil && n > 0 {
		est = max(est, (n+bytesPerToken-1)/bytesPerToken)
	}
	if est == 0 {
		est = defaultTokens
	}
	return min(est, maxTokenEstimate)
}

// NewEvaluator builds an Evaluator. When the cascade engine selects a fallback,
// the request's :path and :authority are rewritten so Envoy proxies it to the
// fallback model/region.
func NewEvaluator(cache *SnapshotCache, engine *governor.CascadeEngine) *Evaluator {
	return &Evaluator{cache: cache, engine: engine, defaultTokens: governor.DefaultRequestTokensEstimate}
}

// ParsePriority maps an X-Request-Priority header value to a Priority. Unknown
// or missing values map to PRIORITY_UNSPECIFIED, which is treated as best effort.
func ParsePriority(v string) governor.Priority {
	p, _ := governor.ParsePriorityName(v)
	return p
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
	tokens := EstimateTokens(headers, e.defaultTokens)
	d := e.engine.Decide(e.cache.Get(), governor.Request{
		Project:  info.ProjectID,
		Region:   info.Region,
		Model:    info.Model,
		Priority: priority,
		Policy:   headers[HeaderPolicy],
		Tokens:   tokens,
	})
	res.Priority = priorityLabel(priority)
	res.Tokens = tokens
	res.OverAllowance = d.OverAllowance
	res.Target = strings.ToLower(firstNonEmpty(d.ReplaceRegion, info.Region) + "/" + firstNonEmpty(d.ReplaceModel, info.Model))

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
