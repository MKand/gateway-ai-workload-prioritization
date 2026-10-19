package dataplane_test

// End-to-end pipeline test without Envoy or GCP:
//
//	YAML config -> LoadConfig -> Reconciler(fake GCP metrics) -> InMemoryStore
//	  -> QuotaDiscoveryService (gRPC) -> SnapshotCache (stream) -> ext_proc server
//
// It validates the three MVP capabilities together: config parsing, quota and
// usage ingestion, and request header mutation.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/dataplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/server"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const pipelineConfig = `
org_id: "123"
project_ids: ["p1"]
regions: ["us-central1", "europe-west1"]
models: ["gemini-3.5-flash"]
poll_interval: 30s
safety_margin: 10
custom_policies:
  fallback-eu:
    cascade:
      - target_model: "gemini-3.5-flash"
        region: "europe-west1"
`

const (
	usPath = "/v1/projects/p1/locations/us-central1/publishers/google/models/gemini-3.5-flash:generateContent"
	euPath = "/v1/projects/p1/locations/europe-west1/publishers/google/models/gemini-3.5-flash:generateContent"
	usHost = "us-central1-aiplatform.googleapis.com"
	euHost = "europe-west1-aiplatform.googleapis.com"
)

// fakeMetrics stands in for the GCP Cloud Quotas + Monitoring client.
type fakeMetrics struct {
	mu        sync.Mutex
	usRPMUsed int64
}

func (f *fakeMetrics) setUSUsage(rpm int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usRPMUsed = rpm
}

func (f *fakeMetrics) FetchMetrics(_ context.Context, _, _, _ []string) (map[string]*controlplane.RawModelQuota, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]*controlplane.RawModelQuota{
		"p1/us-central1/gemini-3.5-flash": {
			ProjectId: "p1", Region: "us-central1", Model: "gemini-3.5-flash",
			MaxRpm: 1000, MaxTpm: 1_000_000, CurrentRpm: f.usRPMUsed,
		},
		"p1/europe-west1/gemini-3.5-flash": {
			ProjectId: "p1", Region: "europe-west1", Model: "gemini-3.5-flash",
			MaxRpm: 1000, MaxTpm: 1_000_000, CurrentRpm: 50,
		},
	}, nil
}

func dialBuf(t *testing.T, lis *bufconn.Listener) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func serve(t *testing.T, register func(*grpc.Server)) *bufconn.Listener {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	register(s)
	go s.Serve(lis)
	t.Cleanup(s.Stop)
	return lis
}

// sendHeaders sends one request-headers message through ext_proc and returns the response.
func sendHeaders(t *testing.T, client extprocv3.ExternalProcessorClient, headers map[string]string) *extprocv3.ProcessingResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Process(ctx)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	hm := &corev3.HeaderMap{}
	for k, v := range headers {
		// Envoy sends raw_value; exercise that path.
		hm.Headers = append(hm.Headers, &corev3.HeaderValue{Key: k, RawValue: []byte(v)})
	}
	if err := stream.Send(&extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{RequestHeaders: &extprocv3.HttpHeaders{Headers: hm}},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	_ = stream.CloseSend()
	return resp
}

func setHeaders(opts []*corev3.HeaderValueOption) map[string]string {
	out := map[string]string{}
	for _, o := range opts {
		out[o.GetHeader().GetKey()] = string(o.GetHeader().GetRawValue())
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPipeline_ConfigToHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Parse config.
	cfgPath := filepath.Join(t.TempDir(), "governor.yaml")
	if err := os.WriteFile(cfgPath, []byte(pipelineConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := governor.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// 2. Control plane: reconcile fake GCP quota/usage into the store and serve it.
	metrics := &fakeMetrics{usRPMUsed: 500} // 50% utilization
	store := controlplane.NewInMemoryStore()
	rec, err := controlplane.NewReconciler(cfg, metrics, store)
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	if err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	qds, err := server.NewServer(store)
	if err != nil {
		t.Fatal(err)
	}
	cpLis := serve(t, func(s *grpc.Server) { pb.RegisterQuotaDiscoveryServiceServer(s, qds) })

	// 3. Data plane: stream snapshots into the cache and serve ext_proc.
	engine, err := governor.NewCascadeEngine(cfg.CustomPolicies)
	if err != nil {
		t.Fatalf("NewCascadeEngine: %v", err)
	}
	cache := dataplane.NewSnapshotCache()
	go cache.Run(ctx, pb.NewQuotaDiscoveryServiceClient(dialBuf(t, cpLis)), "test")

	dpLis := serve(t, func(s *grpc.Server) {
		extprocv3.RegisterExternalProcessorServer(s, dataplane.NewExtProcServer(dataplane.NewEvaluator(cache, engine)))
	})
	client := extprocv3.NewExternalProcessorClient(dialBuf(t, dpLis))

	usUtil := func() float64 {
		s := cache.Get()
		if s == nil || s.ProjectQuotas["p1/us-central1/gemini-3.5-flash"] == nil {
			return -1
		}
		return s.ProjectQuotas["p1/us-central1/gemini-3.5-flash"].UtilizationRpm
	}
	waitFor(t, "initial snapshot", func() bool { return usUtil() == 0.5 })

	// Healthy primary: both priorities forwarded with governor headers.
	for _, prio := range []string{"best_effort", "custom"} {
		resp := sendHeaders(t, client, map[string]string{
			":path": usPath, ":authority": usHost, "x-request-priority": prio, "x-governor-policy": "fallback-eu",
		})
		hr := resp.GetRequestHeaders()
		if hr == nil {
			t.Fatalf("[%s] expected RequestHeaders response, got %T", prio, resp.Response)
		}
		got := setHeaders(hr.GetResponse().GetHeaderMutation().GetSetHeaders())
		if got["x-governor-decision"] != dataplane.DecisionForward {
			t.Errorf("[%s] decision = %q, want forward (reason: %s)", prio, got["x-governor-decision"], got["x-governor-reason"])
		}
	}

	// Usage spikes to 97%: reconcile pushes a new snapshot over the stream.
	metrics.setUSUsage(970)
	if err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	waitFor(t, "saturated snapshot", func() bool { return usUtil() == 0.97 })

	// Best effort is shed with an immediate 429.
	resp := sendHeaders(t, client, map[string]string{":path": usPath, ":authority": usHost, "x-request-priority": "best_effort"})
	ir := resp.GetImmediateResponse()
	if ir == nil {
		t.Fatalf("best_effort: expected ImmediateResponse, got %T", resp.Response)
	}
	if ir.GetStatus().GetCode() != typev3.StatusCode_TooManyRequests {
		t.Errorf("best_effort: status = %v, want 429", ir.GetStatus().GetCode())
	}
	if h := setHeaders(ir.GetHeaders().GetSetHeaders()); h["retry-after"] != "5" || h["x-governor-decision"] != dataplane.DecisionShed {
		t.Errorf("best_effort: 429 headers = %v", h)
	}

	// Custom traffic is rerouted to the EU fallback.
	resp = sendHeaders(t, client, map[string]string{
		":path": usPath, ":authority": usHost, "x-request-priority": "custom", "x-governor-policy": "fallback-eu",
	})
	cr := resp.GetRequestHeaders().GetResponse()
	if cr == nil {
		t.Fatalf("custom: expected RequestHeaders response, got %T", resp.Response)
	}
	got := setHeaders(cr.GetHeaderMutation().GetSetHeaders())
	want := map[string]string{
		"x-governor-decision":      dataplane.DecisionReroute,
		":path":                    euPath,
		":authority":               euHost,
		"x-governor-target-region": "europe-west1",
		"x-governor-original-path": usPath,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("custom: header %q = %q, want %q", k, got[k], v)
		}
	}
	if !cr.GetClearRouteCache() {
		t.Error("custom: ClearRouteCache = false, want true after rewrite")
	}

	// Critical traffic is never modified.
	resp = sendHeaders(t, client, map[string]string{":path": usPath, ":authority": usHost, "x-request-priority": "critical"})
	got = setHeaders(resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders())
	if got["x-governor-decision"] != dataplane.DecisionForward || got[":path"] != "" {
		t.Errorf("critical: headers = %v, want forward with no rewrite", got)
	}
}
