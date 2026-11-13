package dataplane

import (
	"context"
	"io"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/telemetry"
)

// fakeStream replays a fixed list of ext_proc messages for one HTTP stream.
type fakeStream struct {
	grpc.ServerStream
	in   []*extprocv3.ProcessingRequest
	sent []*extprocv3.ProcessingResponse
}

func (f *fakeStream) Context() context.Context { return context.Background() }
func (f *fakeStream) Recv() (*extprocv3.ProcessingRequest, error) {
	if len(f.in) == 0 {
		return nil, io.EOF
	}
	r := f.in[0]
	f.in = f.in[1:]
	return r, nil
}
func (f *fakeStream) Send(r *extprocv3.ProcessingResponse) error {
	f.sent = append(f.sent, r)
	return nil
}

func hdrs(kv map[string]string) *extprocv3.HttpHeaders {
	h := &corev3.HeaderMap{}
	for k, v := range kv {
		h.Headers = append(h.Headers, &corev3.HeaderValue{Key: k, RawValue: []byte(v)})
	}
	return &extprocv3.HttpHeaders{Headers: h}
}

func epReq(priority string) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestHeaders{RequestHeaders: hdrs(map[string]string{
		":path":              "/v1/projects/p1/locations/us-central1/publishers/google/models/m:generateContent",
		":authority":         "us-central1-aiplatform.googleapis.com",
		"x-request-priority": priority,
		"content-length":     "400",
	})}}
}

func respHeaders(status string) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{ResponseHeaders: hdrs(map[string]string{":status": status})}}
}

func metricsServer(t *testing.T, rpm int64) (*ExtProcServer, *telemetry.Registry) {
	t.Helper()
	cfg := &governor.Config{Allowances: map[string]*governor.ModelLimit{"us-central1/m": {MaxRpm: rpm}}}
	engine, err := governor.NewCascadeEngineFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := telemetry.NewRegistry()
	cache := NewSnapshotCache()
	srv := NewExtProcServer(NewEvaluator(cache, engine)).WithMetrics(NewMetrics(reg, cache, engine.Limiter()))
	return srv, reg
}

func scrape(reg *telemetry.Registry) string {
	var b strings.Builder
	reg.WriteText(&b)
	return b.String()
}

func TestExtProc_CountsUpstream429PerTargetAndPriority(t *testing.T) {
	srv, reg := metricsServer(t, 1000)

	for _, tc := range []struct{ prio, status string }{{"critical", "429"}, {"best_effort", "200"}, {"custom", "503"}} {
		s := &fakeStream{in: []*extprocv3.ProcessingRequest{epReq(tc.prio), respHeaders(tc.status)}}
		if err := srv.Process(s); err != nil {
			t.Fatal(err)
		}
		if len(s.sent) != 2 {
			t.Fatalf("%s: sent %d responses, want 2", tc.prio, len(s.sent))
		}
	}

	out := scrape(reg)
	for _, want := range []string{
		`governor_upstream_429_total{target="us-central1/m",priority="critical"} 1`,
		`governor_upstream_responses_total{target="us-central1/m",class="429"} 1`,
		`governor_upstream_responses_total{target="us-central1/m",class="2xx_3xx"} 1`,
		`governor_upstream_responses_total{target="us-central1/m",class="5xx"} 1`,
		`governor_decisions_total{priority="critical",decision="forward"} 1`,
		`governor_estimated_tokens_admitted_total{target="us-central1/m",priority="critical"} 100`,
		`governor_snapshot_age_seconds -1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q\n---\n%s", want, out)
		}
	}
}

func TestExtProc_ShedIsNotCountedAsUpstream429(t *testing.T) {
	srv, reg := metricsServer(t, 1) // capacity 1, best effort stops at 70% -> sheds immediately

	s := &fakeStream{in: []*extprocv3.ProcessingRequest{epReq("best_effort"), respHeaders("429")}}
	if err := srv.Process(s); err != nil {
		t.Fatal(err)
	}
	if s.sent[0].GetImmediateResponse() == nil {
		t.Fatal("expected an immediate 429 shed")
	}
	out := scrape(reg)
	if strings.Contains(out, "governor_upstream_429_total{") {
		t.Errorf("a governor shed was counted as an upstream 429:\n%s", out)
	}
	if !strings.Contains(out, `governor_decisions_total{priority="best_effort",decision="shed"} 1`) {
		t.Errorf("shed decision not counted:\n%s", out)
	}
}

func TestExtProc_CriticalOverAllowanceCounted(t *testing.T) {
	srv, reg := metricsServer(t, 1)
	for i := 0; i < 3; i++ {
		s := &fakeStream{in: []*extprocv3.ProcessingRequest{epReq("critical")}}
		if err := srv.Process(s); err != nil {
			t.Fatal(err)
		}
	}
	if out := scrape(reg); !strings.Contains(out, `governor_critical_over_allowance_total{target="us-central1/m"} 2`) {
		t.Errorf("want 2 over-allowance critical requests:\n%s", out)
	}
}

func TestExtProc_BypassHasNoTarget(t *testing.T) {
	srv, reg := metricsServer(t, 10)
	req := &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestHeaders{RequestHeaders: hdrs(map[string]string{
		":path": "/healthz", ":authority": "us-central1-aiplatform.googleapis.com",
	})}}
	s := &fakeStream{in: []*extprocv3.ProcessingRequest{req, respHeaders("429")}}
	if err := srv.Process(s); err != nil {
		t.Fatal(err)
	}
	out := scrape(reg)
	if !strings.Contains(out, `governor_decisions_total{priority="none",decision="bypass"} 1`) || strings.Contains(out, "governor_upstream_429_total{") {
		t.Errorf("unexpected metrics for bypass:\n%s", out)
	}
}
