package dataplane

import (
	"testing"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
)

const (
	testProject = "p1"
	testModel   = "gemini-3.5-flash"
	usRegion    = "us-central1"
	euRegion    = "europe-west1"
	usHost      = "us-central1-aiplatform.googleapis.com"
	usPath      = "/v1/projects/p1/locations/us-central1/publishers/google/models/gemini-3.5-flash:generateContent"
	euPath      = "/v1/projects/p1/locations/europe-west1/publishers/google/models/gemini-3.5-flash:generateContent"
	euHost      = "europe-west1-aiplatform.googleapis.com"
	testPolicy  = "fallback-eu"
)

// quota builds a ModelQuota with the given utilization on both RPM and TPM.
func quota(region string, util float64) *pb.ModelQuota {
	const maxRPM, maxTPM = 1000, 1_000_000
	curRPM, curTPM := int64(util*maxRPM), int64(util*maxTPM)
	usableRPM, usableTPM := int64(maxRPM*90/100), int64(maxTPM*90/100)
	return &pb.ModelQuota{
		ProjectId: testProject, Model: testModel, Region: region,
		MaxRpm: maxRPM, MaxTpm: maxTPM, CurrentRpm: curRPM, CurrentTpm: curTPM,
		HeadroomRpm: usableRPM - curRPM, HeadroomTpm: usableTPM - curTPM,
		UtilizationRpm: util, UtilizationTpm: util,
	}
}

// snapshot returns a snapshot with the US primary at usUtil and the EU fallback at euUtil.
func snapshot(usUtil, euUtil float64) *pb.QuotaSnapshot {
	return &pb.QuotaSnapshot{
		ProjectQuotas: map[string]*pb.ModelQuota{
			testProject + "/" + usRegion + "/" + testModel: quota(usRegion, usUtil),
			testProject + "/" + euRegion + "/" + testModel: quota(euRegion, euUtil),
		},
	}
}

func newTestEvaluator(t *testing.T, snap *pb.QuotaSnapshot) *Evaluator {
	t.Helper()
	engine, err := governor.NewCascadeEngine(map[string]governor.CustomPolicy{
		testPolicy: {Cascade: []governor.CascadeStep{{TargetModel: testModel, TargetRegion: euRegion}}},
	})
	if err != nil {
		t.Fatalf("NewCascadeEngine: %v", err)
	}
	cache := NewSnapshotCache()
	if snap != nil {
		cache.Set(snap)
	}
	return NewEvaluator(cache, engine)
}

func reqHeaders(priority, policy string) map[string]string {
	h := map[string]string{HeaderPath: usPath, HeaderAuthority: usHost}
	if priority != "" {
		h[HeaderPriority] = priority
	}
	if policy != "" {
		h[HeaderPolicy] = policy
	}
	return h
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name        string
		snap        *pb.QuotaSnapshot
		headers     map[string]string
		wantDec     string
		wantShed    bool
		wantSet     map[string]string // subset of SetHeaders that must match
		wantAbsent  []string          // keys that must NOT be in SetHeaders
		wantRemoved []string          // keys that must be in RemoveHeaders
		wantRoute   bool
	}{
		{
			name:    "non vertex path is bypassed",
			snap:    snapshot(0.1, 0.1),
			headers: map[string]string{HeaderPath: "/healthz", HeaderAuthority: "localhost"},
			wantDec: DecisionBypass,
		},
		{
			name:    "no snapshot yet fails open",
			snap:    nil,
			headers: reqHeaders("best_effort", ""),
			wantDec: DecisionForward,
		},
		{
			name:    "best effort under threshold is forwarded",
			snap:    snapshot(0.5, 0.1),
			headers: reqHeaders("best_effort", ""),
			wantDec: DecisionForward,
		},
		{
			name:     "best effort above 70% is shed with retry-after",
			snap:     snapshot(0.8, 0.1),
			headers:  reqHeaders("best-effort", ""),
			wantDec:  DecisionShed,
			wantShed: true,
			wantSet:  map[string]string{HeaderRetryAfter: "5", HeaderDecision: DecisionShed},
		},
		{
			name:     "missing priority is treated as best effort",
			snap:     snapshot(0.8, 0.1),
			headers:  reqHeaders("", ""),
			wantDec:  DecisionShed,
			wantShed: true,
		},
		{
			name:    "critical is forwarded even when saturated",
			snap:    snapshot(0.97, 0.1),
			headers: reqHeaders("CRITICAL", ""),
			wantDec: DecisionForward,
		},
		{
			name:    "custom with healthy primary is forwarded",
			snap:    snapshot(0.5, 0.1),
			headers: reqHeaders("custom", testPolicy),
			wantDec: DecisionForward,
		},
		{
			name:      "custom saturated reroutes path and authority to fallback",
			snap:      snapshot(0.97, 0.1),
			headers:   reqHeaders("custom", testPolicy),
			wantDec:   DecisionReroute,
			wantRoute: true,
			wantSet: map[string]string{
				HeaderPath:         euPath,
				HeaderAuthority:    euHost,
				HeaderOriginalPath: usPath,
				HeaderTargetModel:  testModel,
				HeaderTargetRegion: euRegion,
			},
		},
		{
			name:     "custom saturated with unknown policy is shed",
			snap:     snapshot(0.97, 0.1),
			headers:  reqHeaders("custom", "nope"),
			wantDec:  DecisionShed,
			wantShed: true,
		},
		{
			name:     "custom saturated with saturated fallback is shed",
			snap:     snapshot(0.97, 0.99),
			headers:  reqHeaders("custom", testPolicy),
			wantDec:  DecisionShed,
			wantShed: true,
		},
		{
			name: "client supplied governor headers are stripped on forward",
			snap: snapshot(0.1, 0.1),
			headers: func() map[string]string {
				h := reqHeaders("critical", "")
				h[HeaderTargetRegion] = "spoofed"
				return h
			}(),
			wantDec:     DecisionForward,
			wantAbsent:  []string{HeaderTargetRegion},
			wantRemoved: []string{HeaderTargetRegion, HeaderTargetModel, HeaderOriginalPath},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newTestEvaluator(t, tc.snap).Evaluate(tc.headers)

			if got.Decision != tc.wantDec {
				t.Errorf("Decision = %q, want %q (reason: %s)", got.Decision, tc.wantDec, got.SetHeaders[HeaderReason])
			}
			if got.Shed != tc.wantShed {
				t.Errorf("Shed = %v, want %v", got.Shed, tc.wantShed)
			}
			if got.RouteChanged != tc.wantRoute {
				t.Errorf("RouteChanged = %v, want %v", got.RouteChanged, tc.wantRoute)
			}
			if got.SetHeaders[HeaderDecision] != tc.wantDec {
				t.Errorf("%s header = %q, want %q", HeaderDecision, got.SetHeaders[HeaderDecision], tc.wantDec)
			}
			if got.SetHeaders[HeaderReason] == "" {
				t.Errorf("%s header is empty", HeaderReason)
			}
			for k, want := range tc.wantSet {
				if got.SetHeaders[k] != want {
					t.Errorf("SetHeaders[%q] = %q, want %q", k, got.SetHeaders[k], want)
				}
			}
			for _, k := range tc.wantAbsent {
				if v, ok := got.SetHeaders[k]; ok {
					t.Errorf("SetHeaders[%q] = %q, want absent", k, v)
				}
			}
			for _, k := range tc.wantRemoved {
				if !contains(got.RemoveHeaders, k) {
					t.Errorf("RemoveHeaders %v missing %q", got.RemoveHeaders, k)
				}
			}
			for k := range got.SetHeaders {
				if contains(got.RemoveHeaders, k) {
					t.Errorf("header %q is both set and removed", k)
				}
			}
		})
	}
}

func TestParsePriority(t *testing.T) {
	cases := map[string]governor.Priority{
		"critical":    governor.Priority_PRIORITY_CRITICAL,
		" Critical ":  governor.Priority_PRIORITY_CRITICAL,
		"best_effort": governor.Priority_PRIORITY_BEST_EFFORT,
		"best-effort": governor.Priority_PRIORITY_BEST_EFFORT,
		"custom":      governor.Priority_PRIORITY_CUSTOM,
		"":            governor.Priority_PRIORITY_UNSPECIFIED,
		"urgent":      governor.Priority_PRIORITY_UNSPECIFIED,
	}
	for in, want := range cases {
		if got := ParsePriority(in); got != want {
			t.Errorf("ParsePriority(%q) = %v, want %v", in, got, want)
		}
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
