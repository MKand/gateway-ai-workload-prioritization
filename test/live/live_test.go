//go:build live

// Package live is the full integration test against real GCP: real Cloud
// Quotas + Cloud Monitoring reads, the real control plane / data plane code,
// the real Envoy config, and real Vertex AI traffic.
//
// Do not run directly; use test/live/run-single.sh or test/live/run-dual.sh,
// which prepare GCP (lowered quota on project A), export GQG_* variables and
// tear everything down afterwards.
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/dataplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/server"
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

	// Monitoring usage metrics lag real traffic by a few minutes.
	quotaApplyTimeout = 15 * time.Minute
	saturateTimeout   = 12 * time.Minute
	idleTimeout       = 6 * time.Minute
	pollEvery         = 15 * time.Second
)

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

type testEnv struct {
	config, projectA, projectB, model, primary, fallback, envoyContainer string
	lowered                                                              int64
}

func (e testEnv) dual() bool { return e.projectB != "" }

func loadEnv(t *testing.T) testEnv {
	t.Helper()
	if os.Getenv("GQG_CONFIG") == "" {
		t.Skip("GQG_CONFIG not set; run via test/live/run-single.sh or run-dual.sh")
	}
	lowered, err := strconv.ParseInt(os.Getenv("GQG_LOWERED_RPM"), 10, 64)
	if err != nil {
		t.Fatalf("GQG_LOWERED_RPM: %v", err)
	}
	return testEnv{
		config:         os.Getenv("GQG_CONFIG"),
		projectA:       os.Getenv("GQG_PROJECT_A"),
		projectB:       os.Getenv("GQG_PROJECT_B"),
		model:          os.Getenv("GQG_MODEL"),
		primary:        os.Getenv("GQG_REGION_PRIMARY"),
		fallback:       os.Getenv("GQG_REGION_FALLBACK"),
		envoyContainer: os.Getenv("GQG_ENVOY_CONTAINER"),
		lowered:        lowered,
	}
}

func host(region string) string { return region + "-aiplatform.googleapis.com" }

// ---------------------------------------------------------------------------
// Stack: control plane (real GCP clients) + data plane + Envoy
// ---------------------------------------------------------------------------

type stack struct {
	env      testEnv
	store    *controlplane.InMemoryStore
	rec      *controlplane.Reconciler
	cache    *dataplane.SnapshotCache
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
	gcp, err := controlplane.NewGCPClient(ctx, cfg.OrgId, cfg.DefaultProjectLimits, cfg.DefaultOrgLimits, quotaClient, usageClient)
	if err != nil {
		t.Fatalf("gcp client: %v", err)
	}

	s := &stack{env: env, store: controlplane.NewInMemoryStore(), cache: dataplane.NewSnapshotCache()}
	if s.rec, err = controlplane.NewReconciler(cfg, gcp, s.store); err != nil {
		t.Fatal(err)
	}
	if s.tokens, err = google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform"); err != nil {
		t.Fatalf("ADC token source: %v", err)
	}

	// Control plane gRPC (plaintext on loopback; TLS is covered by pkg/transport tests).
	qds, _ := server.NewServer(s.store)
	s.cpServer = serveGRPC(t, cpAddr, func(g *grpc.Server) { pb.RegisterQuotaDiscoveryServiceServer(g, qds) })

	// Data plane.
	engine, err := governor.NewCascadeEngine(cfg.CustomPolicies)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(cpAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go s.cache.Run(ctx, pb.NewQuotaDiscoveryServiceClient(conn), "live-test")
	s.dpServer = serveGRPC(t, dpAddr, func(g *grpc.Server) {
		extprocv3.RegisterExternalProcessorServer(g, dataplane.NewExtProcServer(dataplane.NewEvaluator(s.cache, engine)))
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

// refresh runs one reconcile against real GCP and returns the new snapshot.
// The store pushes it to the data plane over the gRPC stream.
func (s *stack) refresh(t *testing.T, ctx context.Context) *pb.QuotaSnapshot {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.rec.ReconcileOnce(rctx); err != nil {
		t.Logf("reconcile error (will retry): %v", err)
		return nil
	}
	snap, _ := s.store.Get(ctx)
	return snap
}

func (s *stack) projectQuota(snap *pb.QuotaSnapshot, project, region string) *pb.ModelQuota {
	if snap == nil {
		return nil
	}
	return snap.ProjectQuotas[strings.ToLower(project+"/"+region+"/"+s.env.model)]
}

func (s *stack) orgQuota(snap *pb.QuotaSnapshot, region string) *pb.ModelQuota {
	if snap == nil {
		return nil
	}
	return snap.OrgQuotas[strings.ToLower(region+"/"+s.env.model)]
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

// do issues a minimal generateContent call through Envoy. Safe to call from
// any goroutine (never calls t.Fatal).
func (s *stack) do(project, region, priority, policy string) (result, error) {
	req, err := http.NewRequest(http.MethodPost, envoyURL+modelPath(project, region, s.env.model), strings.NewReader(requestBody(s.env.model)))
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

// send is do() for the test goroutine: it fails the test on transport errors.
func (s *stack) send(t *testing.T, project, region, priority, policy string) result {
	t.Helper()
	r, err := s.do(project, region, priority, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("evidence: %s %s priority=%q policy=%q -> HTTP %d decision=%q retry-after=%q",
		project, region, priority, policy, r.status, r.headers.Get("X-Governor-Decision"), r.headers.Get("Retry-After"))
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
				t.Logf("evidence: %s: %s (after %v)", what, detail, timeout-time.Until(deadline))
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

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func describe(q *pb.ModelQuota) string {
	if q == nil {
		return "no quota entry"
	}
	return fmt.Sprintf("max_rpm=%d current_rpm=%d util=%.2f", q.MaxRpm, q.CurrentRpm, q.UtilizationRpm)
}

// load sends background traffic until stop is closed: lowered+2 critical
// requests per minute to project A's primary region (enough to saturate it),
// plus one best-effort request per minute to project B's primary (dual mode)
// so B has non-zero usage for the org-total check.
func (s *stack) load(t *testing.T, stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	perMinute := s.env.lowered + 2
	tick := time.NewTicker(time.Minute / time.Duration(perMinute))
	defer tick.Stop()
	var n int64
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			r, err := s.do(s.env.projectA, s.env.primary, "critical", "")
			t.Logf("load: project A critical -> %d %v", r.status, errOrEmpty(err))
			n++
			if s.env.dual() && n%perMinute == 0 {
				r, err := s.do(s.env.projectB, s.env.primary, "best_effort", "")
				t.Logf("load: project B best_effort -> %d %v", r.status, errOrEmpty(err))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

func TestLive(t *testing.T) {
	env := loadEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := startStack(t, ctx, env)
	projects := []string{env.projectA}
	if env.dual() {
		projects = append(projects, env.projectB)
	}

	// ---- L3: live reads -------------------------------------------------
	readsOK := t.Run("R1 reconcile against real GCP", func(t *testing.T) {
		waitFor(t, "first successful reconcile", 2*time.Minute, pollEvery, func() (bool, string) {
			if s.refresh(t, ctx) == nil {
				return false, "reconcile failed"
			}
			return true, "ok"
		})
	})

	readsOK = t.Run("R2 lowered limit visible on project A", func(t *testing.T) {
		waitFor(t, "lowered quota to apply", quotaApplyTimeout, pollEvery, func() (bool, string) {
			q := s.projectQuota(s.refresh(t, ctx), env.projectA, env.primary)
			return q != nil && q.MaxRpm == env.lowered, describe(q)
		})
	}) && readsOK

	readsOK = t.Run("R3 every project/region has real limits", func(t *testing.T) {
		snap := s.refresh(t, ctx)
		for _, p := range projects {
			for _, r := range []string{env.primary, env.fallback} {
				q := s.projectQuota(snap, p, r)
				if q == nil || q.MaxRpm <= 0 {
					t.Errorf("%s/%s: %s", p, r, describe(q))
				} else if q.MaxRpm == controlplane.FallbackMaxRPM {
					t.Errorf("%s/%s: max_rpm equals the built-in fallback (%d); the real limit was not read from Cloud Quotas", p, r, q.MaxRpm)
				}
			}
		}
		if q := s.projectQuota(snap, env.projectA, env.fallback); q != nil && q.MaxRpm <= env.lowered {
			t.Errorf("fallback region limit should be above the lowered primary limit: %s", describe(q))
		}
	}) && readsOK
	if !readsOK {
		t.Fatal("live reads failed; skipping traffic scenarios")
	}

	// ---- L4: full stack -------------------------------------------------
	startEnvoy(t, env)

	t.Run("wait for primary to be idle", func(t *testing.T) {
		waitFor(t, "project A primary utilization < 70%", idleTimeout, pollEvery, func() (bool, string) {
			q := s.projectQuota(s.refresh(t, ctx), env.projectA, env.primary)
			return q != nil && q.UtilizationRpm < governor.DefaultShedThresholdBestEffort, describe(q)
		})
	})

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

	t.Run("F1 healthy best effort forwarded to primary", func(t *testing.T) {
		r := s.send(t, env.projectA, env.primary, "best_effort", "")
		l := s.logFor(t, r.requestID)
		check(t, r.status, http.StatusOK, "status")
		check(t, l.Authority, host(env.primary), "dialed host")
		check(t, l.Decision, dataplane.DecisionForward, "decision")
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go s.load(t, stop, &wg)
	stopLoad := func() {
		select {
		case <-stop:
		default:
			close(stop)
			wg.Wait()
		}
	}
	defer stopLoad()

	saturated := t.Run("F3 saturate project A primary", func(t *testing.T) {
		waitFor(t, "project A primary utilization >= 95%", saturateTimeout, pollEvery, func() (bool, string) {
			q := s.projectQuota(s.refresh(t, ctx), env.projectA, env.primary)
			return q != nil && governor.IsSaturated(q), describe(q)
		})
		waitFor(t, "data plane to receive saturated snapshot", 10*time.Second, 100*time.Millisecond, func() (bool, string) {
			q := s.projectQuota(s.cache.Get(), env.projectA, env.primary)
			return q != nil && governor.IsSaturated(q), describe(q)
		})
	})
	if !saturated {
		t.Fatal("could not saturate the primary region; skipping saturated scenarios")
	}

	t.Run("F4 best effort shed with 429", func(t *testing.T) {
		r := s.send(t, env.projectA, env.primary, "best_effort", "")
		l := s.logFor(t, r.requestID)
		check(t, r.status, http.StatusTooManyRequests, "status")
		check(t, r.headers.Get("X-Governor-Decision"), dataplane.DecisionShed, "decision header")
		if r.headers.Get("Retry-After") == "" {
			t.Error("missing Retry-After")
		}
		check(t, l.UpstreamHost, "", "upstream host (no upstream call expected)")
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

	t.Run("F6 critical never rewritten", func(t *testing.T) {
		r := s.send(t, env.projectA, env.primary, "critical", "")
		l := s.logFor(t, r.requestID)
		check(t, l.Authority, host(env.primary), "dialed host")
		check(t, l.Decision, dataplane.DecisionForward, "decision")
		if r.status != http.StatusOK && r.status != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 200 or Google's own 429", r.status)
		}
	})

	if env.dual() {
		t.Run("M1 project B unaffected by project A saturation", func(t *testing.T) {
			r := s.send(t, env.projectB, env.primary, "custom", policyName)
			l := s.logFor(t, r.requestID)
			check(t, r.status, http.StatusOK, "status")
			check(t, l.Authority, host(env.primary), "dialed host")
			check(t, l.Decision, dataplane.DecisionForward, "decision")
		})

		t.Run("M2 org usage equals A + B", func(t *testing.T) {
			waitFor(t, "project B usage to appear", idleTimeout, pollEvery, func() (bool, string) {
				snap := s.refresh(t, ctx)
				a := s.projectQuota(snap, env.projectA, env.primary)
				b := s.projectQuota(snap, env.projectB, env.primary)
				org := s.orgQuota(snap, env.primary)
				if a == nil || b == nil || org == nil {
					return false, "missing quota entries"
				}
				detail := fmt.Sprintf("org=%d a=%d b=%d", org.CurrentRpm, a.CurrentRpm, b.CurrentRpm)
				if b.CurrentRpm == 0 {
					return false, detail
				}
				if org.CurrentRpm != a.CurrentRpm+b.CurrentRpm {
					t.Errorf("org usage != A + B: %s", detail)
				}
				return true, detail
			})
		})
	}

	stopLoad()

	// ---- Failure modes --------------------------------------------------
	t.Run("X2 control plane down: data plane keeps last snapshot", func(t *testing.T) {
		s.cpServer.Stop()
		r := s.send(t, env.projectA, env.primary, "best_effort", "")
		check(t, r.status, http.StatusTooManyRequests, "status")
		check(t, r.headers.Get("X-Governor-Decision"), dataplane.DecisionShed, "decision header")
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
