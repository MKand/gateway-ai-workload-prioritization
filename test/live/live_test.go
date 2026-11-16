//go:build live

// Package live is the full integration test against real GCP: real Cloud
// Quotas and Cloud Monitoring reads, the real control plane and data plane
// code, the real Envoy config, and real Vertex AI traffic.
//
// The governor enforces a configured allowance itself, so the test needs no
// quota changes in GCP: setup.sh writes a deliberately low allowance (default
// 20 RPM) and the test drives traffic against it. Any callable Gemini model
// works, including models on Dynamic Shared Quota.
//
// Do not run directly; use test/live/run-single.sh or test/live/run-dual.sh,
// which prepare GCP, export GQG_* variables and tear everything down
// afterwards.
package live

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/dataplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/server"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/telemetry"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	envoyImage    = "envoyproxy/envoy:v1.34-latest"
	envoyURL      = "http://127.0.0.1:10000"
	envoyReadyURL = "http://127.0.0.1:9901/ready"
	cpAddr        = "127.0.0.1:50051"
	dpAddr        = "127.0.0.1:50052"
	policyName    = "fallback" // written by setup.sh

	// Cloud Monitoring usage metrics lag real traffic by a few minutes.
	bypassTimeout = 10 * time.Minute
	pollEvery     = 20 * time.Second
)

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

type testEnv struct {
	config, projectA, projectB, model, primary, fallback, envoyContainer string
	allowanceRPM                                                         int64
	directCalls                                                          int
}

func (e testEnv) dual() bool { return e.projectB != "" }

func loadEnv(t *testing.T) testEnv {
	t.Helper()
	if os.Getenv("GQG_CONFIG") == "" {
		t.Skip("GQG_CONFIG not set; run via test/live/run-single.sh or run-dual.sh")
	}
	allowance, err := strconv.ParseInt(os.Getenv("GQG_ALLOWANCE_RPM"), 10, 64)
	if err != nil || allowance < 4 {
		t.Fatalf("GQG_ALLOWANCE_RPM must be an integer >= 4: %q", os.Getenv("GQG_ALLOWANCE_RPM"))
	}
	direct := 10
	if v := os.Getenv("GQG_DIRECT_CALLS"); v != "" {
		if direct, err = strconv.Atoi(v); err != nil || direct < 1 {
			t.Fatalf("GQG_DIRECT_CALLS must be a positive integer: %q", v)
		}
	}
	return testEnv{
		config:         os.Getenv("GQG_CONFIG"),
		projectA:       os.Getenv("GQG_PROJECT_A"),
		projectB:       os.Getenv("GQG_PROJECT_B"),
		model:          os.Getenv("GQG_MODEL"),
		primary:        os.Getenv("GQG_REGION_PRIMARY"),
		fallback:       os.Getenv("GQG_REGION_FALLBACK"),
		envoyContainer: os.Getenv("GQG_ENVOY_CONTAINER"),
		allowanceRPM:   allowance,
		directCalls:    direct,
	}
}

func host(region string) string { return router(region) }

// router mirrors pkg/router.HostFor without importing it, so a regression in
// the router is caught by the test rather than hidden by it.
func router(region string) string {
	if region == "global" {
		return "aiplatform.googleapis.com"
	}
	return region + "-aiplatform.googleapis.com"
}

// ---------------------------------------------------------------------------
// Stack: control plane (real GCP clients) + data plane + Envoy
// ---------------------------------------------------------------------------

type stack struct {
	env      testEnv
	cfg      *governor.Config
	gcp      *controlplane.GCPClient
	store    *controlplane.InMemoryStore
	rec      *controlplane.Reconciler
	bypass   *controlplane.BypassTracker
	cache    *dataplane.SnapshotCache
	reg      *telemetry.Registry
	cpServer *grpc.Server
	dpServer *grpc.Server
	tokens   oauth2.TokenSource
}

func startStack(t *testing.T, ctx context.Context, env testEnv) *stack {
	t.Helper()
	cfg, err := governor.LoadConfig(env.config)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	quotaClient, err := controlplane.NewGCPQuotaRequestClient(ctx)
	if err != nil {
		t.Fatalf("quota client: %v", err)
	}
	t.Cleanup(func() { quotaClient.Close() })
	usageClient, err := controlplane.NewGCPUsageRequestClient(ctx)
	if err != nil {
		t.Fatalf("usage client: %v", err)
	}
	t.Cleanup(func() { usageClient.Close() })

	s := &stack{env: env, cfg: cfg, store: controlplane.NewInMemoryStore(), cache: dataplane.NewSnapshotCache(), reg: telemetry.NewRegistry()}
	if s.gcp, err = controlplane.NewGCPClient(ctx, cfg.DefaultProjectLimits, cfg.Allowances, quotaClient, usageClient); err != nil {
		t.Fatalf("gcp client: %v", err)
	}
	if s.rec, err = controlplane.NewReconciler(cfg, s.gcp, s.store); err != nil {
		t.Fatal(err)
	}
	if s.tokens, err = google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform"); err != nil {
		t.Fatalf("ADC token source: %v", err)
	}

	// Control plane gRPC (plaintext on loopback; TLS is covered by pkg/transport tests).
	s.bypass = controlplane.NewBypassTracker(cfg, nil)
	qds, _ := server.NewServer(s.store)
	qds.WithBypassTracker(s.bypass)
	s.cpServer = serveGRPC(t, cpAddr, func(g *grpc.Server) { pb.RegisterQuotaDiscoveryServiceServer(g, qds) })

	// Data plane, wired like cmd/dataplane: local limiter, snapshot stream,
	// usage reporter (every 2s so the test is quick) and metrics.
	engine, err := governor.NewCascadeEngineFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(cpAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	client := pb.NewQuotaDiscoveryServiceClient(conn)
	go s.cache.Run(ctx, client, "live-test")
	go dataplane.NewUsageReporter(client, engine.Limiter(), "live-test", 2*time.Second).Run(ctx)
	metrics := dataplane.NewMetrics(s.reg, s.cache, engine.Limiter())
	s.dpServer = serveGRPC(t, dpAddr, func(g *grpc.Server) {
		evaluator := dataplane.NewEvaluator(s.cache, engine).WithDefaultTokens(cfg.RequestTokens())
		extprocv3.RegisterExternalProcessorServer(g, dataplane.NewExtProcServer(evaluator).WithMetrics(metrics))
	})
	return s
}

func serveGRPC(t *testing.T, addr string, register func(*grpc.Server)) *grpc.Server {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	g := grpc.NewServer()
	register(g)
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	return g
}

func startEnvoy(t *testing.T, env testEnv) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	cfg, _ := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "envoy", "envoy.yaml"))
	_ = exec.Command("docker", "rm", "-f", env.envoyContainer).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", env.envoyContainer, "--network", "host",
		"-v", cfg+":/etc/envoy/envoy.yaml:ro", envoyImage, "-c", "/etc/envoy/envoy.yaml", "-l", "warn",
		// Access logs are flushed every 10s by default; flush every 500ms so the test can read them promptly.
		"--file-flush-interval-msec", "500").CombinedOutput(); err != nil {
		t.Fatalf("docker run envoy: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", env.envoyContainer).Run() })
	waitFor(t, "envoy ready", 30*time.Second, time.Second, func() (bool, string) {
		resp, err := http.Get(envoyReadyURL)
		if err != nil {
			return false, err.Error()
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK, resp.Status
	})
}

func (s *stack) target(region string) string {
	return strings.ToLower(region + "/" + s.env.model)
}

func (s *stack) scrape() string {
	var b strings.Builder
	s.reg.WriteText(&b)
	return b.String()
}

// metric returns the value of one metric line (name plus labels exactly as
// rendered), or 0.
func (s *stack) metric(series string) float64 {
	for _, line := range strings.Split(s.scrape(), "\n") {
		if strings.HasPrefix(line, series+" ") {
			v, _ := strconv.ParseFloat(strings.TrimPrefix(line, series+" "), 64)
			return v
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Requests and Envoy access log
// ---------------------------------------------------------------------------

type result struct {
	status    int
	requestID string
	headers   http.Header
}

// accessLog is one Envoy JSON access log line. Envoy's json_format emits
// missing values as null (decoded here as "") and numeric fields as numbers.
type accessLog struct {
	RequestID     string `json:"request_id"`
	Authority     string `json:"authority"`
	Path          string `json:"path"`
	Decision      string `json:"decision"`
	OriginalPath  string `json:"original_path"`
	ResponseCode  int    `json:"response_code"`
	ResponseFlags string `json:"response_flags"`
	UpstreamHost  string `json:"upstream_host"`
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "gqg-live-" + hex.EncodeToString(b)
}

// requestBody returns the smallest valid generateContent body for the model.
// TTS models reject text-only output and need an audio response config.
// GQG_REQUEST_BODY overrides both.
func requestBody(model string) string {
	if b := os.Getenv("GQG_REQUEST_BODY"); b != "" {
		return b
	}
	if strings.HasSuffix(model, "-tts") {
		return `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseModalities":["AUDIO"],` +
			`"speechConfig":{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"Kore"}}}}}`
	}
	return `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":1}}`
}

func modelPath(project, region, model string) string {
	return fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent", project, region, model)
}

// call issues a minimal generateContent request to baseURL (Envoy, or Vertex
// AI directly). Safe to call from any goroutine (never calls t.Fatal).
func (s *stack) call(baseURL, project, region, priority, policy string) (result, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+modelPath(project, region, s.env.model), strings.NewReader(requestBody(s.env.model)))
	if err != nil {
		return result{}, err
	}
	req.Host = host(region)
	tok, err := s.tokens.Token()
	if err != nil {
		return result{}, fmt.Errorf("token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	id := newRequestID()
	req.Header.Set("X-Request-Id", id)
	if priority != "" {
		req.Header.Set("X-Request-Priority", priority)
	}
	if policy != "" {
		req.Header.Set("X-Governor-Policy", policy)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return result{requestID: id}, fmt.Errorf("request %s: %w", id, err)
	}
	defer resp.Body.Close()
	return result{status: resp.StatusCode, requestID: id, headers: resp.Header}, nil
}

// send calls through Envoy and fails the test on transport errors.
func (s *stack) send(t *testing.T, project, region, priority, policy string) result {
	t.Helper()
	r, err := s.call(envoyURL, project, region, priority, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("evidence: %s %s priority=%q policy=%q -> HTTP %d decision=%q reason=%q",
		project, region, priority, policy, r.status, r.headers.Get("X-Governor-Decision"), r.headers.Get("X-Governor-Reason"))
	return r
}

// logFor returns Envoy's access log line for a request ID.
func (s *stack) logFor(t *testing.T, id string) accessLog {
	t.Helper()
	var found accessLog
	waitFor(t, "access log for "+id, 30*time.Second, 200*time.Millisecond, func() (bool, string) {
		out, _ := exec.Command("docker", "logs", s.env.envoyContainer).Output()
		sc := bufio.NewScanner(bytes.NewReader(out))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.Contains(line, []byte(id)) {
				continue
			}
			if err := json.Unmarshal(line, &found); err != nil {
				return false, "unparseable log line: " + err.Error()
			}
			if found.RequestID == id {
				t.Logf("evidence: envoy access log: %s", line)
				return true, ""
			}
		}
		return false, "not yet logged"
	})
	return found
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func waitFor(t *testing.T, what string, timeout, every time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, detail := cond()
		if ok {
			if detail != "" {
				t.Logf("evidence: %s: %s (after %v)", what, detail, (timeout - time.Until(deadline)).Round(time.Second))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s (last: %s)", timeout, what, detail)
		}
		time.Sleep(every)
	}
}

func check(t *testing.T, got, want any, what string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func describeSeries(series controlplane.UsageSeries) string {
	if len(series) == 0 {
		return "no usage series"
	}
	var parts []string
	for target, minutes := range series {
		keys := make([]int64, 0, len(minutes))
		for m := range minutes {
			keys = append(keys, m)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		var pts []string
		for _, m := range keys {
			pts = append(pts, fmt.Sprintf("%s=%d", time.Unix(m, 0).UTC().Format("15:04"), minutes[m].Requests))
		}
		parts = append(parts, target+": "+strings.Join(pts, " "))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

func TestLive(t *testing.T) {
	env := loadEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := startStack(t, ctx, env)
	primary := s.target(env.primary)

	// ---- Reads against real GCP ----------------------------------------
	readsOK := t.Run("R1 reconcile against real GCP", func(t *testing.T) {
		waitFor(t, "first successful reconcile", 2*time.Minute, 15*time.Second, func() (bool, string) {
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := s.rec.ReconcileOnce(rctx); err != nil {
				return false, err.Error()
			}
			return true, "ok"
		})
	})
	readsOK = t.Run("R2 Cloud Monitoring usage series readable", func(t *testing.T) {
		series, err := s.gcp.FetchUsageSeries(ctx, s.cfg.MetricsScopeProject, s.cfg.ProjectIDs, s.cfg.Regions, s.cfg.Models)
		if err != nil {
			t.Fatalf("FetchUsageSeries: %v", err)
		}
		t.Logf("evidence: current usage: %s", describeSeries(series))
	}) && readsOK
	if !readsOK {
		t.Fatal("live reads failed; skipping traffic scenarios")
	}

	// ---- Admission through real Envoy -----------------------------------
	startEnvoy(t, env)

	t.Run("F2 non vertex host rejected", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, envoyURL+"/", nil)
		req.Host = "example.com"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		check(t, resp.StatusCode, http.StatusNotFound, "status")
	})

	t.Run("F1 best effort forwarded to primary", func(t *testing.T) {
		r := s.send(t, env.projectA, env.primary, "best_effort", "")
		l := s.logFor(t, r.requestID)
		check(t, r.status, http.StatusOK, "status")
		check(t, l.Authority, host(env.primary), "dialed host")
		check(t, l.Decision, dataplane.DecisionForward, "decision")
		waitFor(t, "upstream 2xx counted", 5*time.Second, 100*time.Millisecond, func() (bool, string) {
			v := s.metric(fmt.Sprintf(`governor_upstream_responses_total{target=%q,class="2xx_3xx"}`, primary))
			return v >= 1, fmt.Sprintf("%v", v)
		})
	})

	shed := t.Run("F3 best effort stops at the critical reserve", func(t *testing.T) {
		// With allowance A and a 30% reserve, best effort gets about 70% of A
		// (plus whatever refills while the requests run).
		admitted := 1 // F1
		for i := 0; i < int(env.allowanceRPM)*2; i++ {
			r := s.send(t, env.projectA, env.primary, "best_effort", "")
			if r.headers.Get("X-Governor-Decision") != dataplane.DecisionShed {
				admitted++
				continue
			}
			l := s.logFor(t, r.requestID)
			check(t, r.status, http.StatusTooManyRequests, "status")
			if r.headers.Get("Retry-After") == "" {
				t.Error("missing Retry-After")
			}
			check(t, l.UpstreamHost, "", "upstream host (no upstream call expected)")
			t.Logf("evidence: best effort shed after %d admitted requests (allowance %d RPM, reserve %d%%)",
				admitted, env.allowanceRPM, s.cfg.ReservePercent())
			if limit := env.allowanceRPM; int64(admitted) > limit {
				t.Errorf("best effort admitted %d requests, more than the whole %d RPM allowance", admitted, limit)
			}
			return
		}
		t.Fatalf("best effort was never shed after %d requests", env.allowanceRPM*2)
	})
	if !shed {
		t.Fatal("best effort was not shed; skipping the scenarios that need a full allowance")
	}

	t.Run("F4 critical still forwarded while best effort is shed", func(t *testing.T) {
		r := s.send(t, env.projectA, env.primary, "critical", "")
		l := s.logFor(t, r.requestID)
		check(t, l.Authority, host(env.primary), "dialed host")
		check(t, l.Decision, dataplane.DecisionForward, "decision")
		if r.status != http.StatusOK && r.status != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 200 or Google's own 429", r.status)
		}
	})

	t.Run("F5 custom rerouted to fallback region", func(t *testing.T) {
		r := s.send(t, env.projectA, env.primary, "custom", policyName)
		l := s.logFor(t, r.requestID)
		check(t, r.status, http.StatusOK, "status")
		check(t, l.Authority, host(env.fallback), "dialed host")
		check(t, l.Path, modelPath(env.projectA, env.fallback, env.model), "rewritten path")
		check(t, l.Decision, dataplane.DecisionReroute, "decision")
		check(t, l.OriginalPath, modelPath(env.projectA, env.primary, env.model), "original path")
	})

	if env.dual() {
		t.Run("M1 allowance is org-wide: project B shares project A's budget", func(t *testing.T) {
			r := s.send(t, env.projectB, env.primary, "best_effort", "")
			check(t, r.headers.Get("X-Governor-Decision"), dataplane.DecisionShed, "decision")
		})
	}

	t.Run("F6 metrics recorded the decisions", func(t *testing.T) {
		for series, min := range map[string]float64{
			`governor_decisions_total{priority="best_effort",decision="shed"}`: 1,
			`governor_decisions_total{priority="critical",decision="forward"}`: 1,
			`governor_decisions_total{priority="custom",decision="reroute"}`:   1,
		} {
			if v := s.metric(series); v < min {
				t.Errorf("%s = %v, want >= %v", series, v, min)
			}
		}
		t.Logf("evidence: metrics:\n%s", s.scrape())
	})

	// ---- Bypass subtraction ---------------------------------------------
	t.Run("B1 direct calls to Vertex AI show up as bypass", func(t *testing.T) {
		for i := 0; i < env.directCalls; i++ {
			r, err := s.call("https://"+host(env.primary), env.projectA, env.primary, "", "")
			if err != nil {
				t.Fatalf("direct call: %v", err)
			}
			t.Logf("evidence: direct call to Vertex AI (not through the governor) -> HTTP %d", r.status)
		}
		waitFor(t, "Cloud Monitoring to show the bypass traffic", bypassTimeout, pollEvery, func() (bool, string) {
			series, err := s.gcp.FetchUsageSeries(ctx, s.cfg.MetricsScopeProject, s.cfg.ProjectIDs, s.cfg.Regions, s.cfg.Models)
			if err != nil {
				return false, err.Error()
			}
			s.bypass.Update(series)
			e := s.bypass.Estimate(primary)
			return e.BypassRequests > 0, fmt.Sprintf("bypass=%d req, rpm factor=%.2f; usage %s",
				e.BypassRequests, e.RPMFactor, describeSeries(series))
		})
	})

	// ---- Failure modes --------------------------------------------------
	t.Run("X2 control plane down: data plane keeps enforcing locally", func(t *testing.T) {
		s.cpServer.Stop()
		r := s.send(t, env.projectA, env.primary, "best_effort", "")
		if d := r.headers.Get("X-Governor-Decision"); d != dataplane.DecisionForward && d != dataplane.DecisionShed {
			t.Errorf("decision = %q, want a governor decision (forward or shed)", d)
		}
	})

	t.Run("X1 data plane down: Envoy fails open", func(t *testing.T) {
		s.dpServer.Stop()
		r := s.send(t, env.projectA, env.fallback, "best_effort", "")
		l := s.logFor(t, r.requestID)
		check(t, r.status, http.StatusOK, "status")
		check(t, l.Authority, host(env.fallback), "dialed host")
		check(t, l.Decision, "", "decision (governor bypassed)")
	})
}
