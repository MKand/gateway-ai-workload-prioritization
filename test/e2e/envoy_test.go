//go:build e2e

// Package e2e runs the governor behind a real Envoy (Docker) using the real
// deploy/envoy/envoy.yaml, and proves that rerouted requests are actually
// delivered to a different regional endpoint.
//
// Topology (Docker network "governor-e2e"):
//
//	test client --> Envoy :10000 --ext_proc--> governor (this process, :50052)
//	                  |
//	                  +--TLS--> us-central1-aiplatform.googleapis.com   (fakevertex --region us-central1)
//	                  +--TLS--> europe-west1-aiplatform.googleapis.com  (fakevertex --region europe-west1)
//
// The fake endpoints use the real Vertex AI hostnames via Docker network
// aliases and a test CA, so Envoy's DNS + TLS + SNI path is exercised. GCP
// quota/usage is replaced by a fake metrics client.
//
// Run with: make e2e  (requires Docker; uses ports 10000, 9901, 50051, 50052)
package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	envoyImage  = "envoyproxy/envoy:v1.34-latest"
	network     = "governor-e2e"
	envoyName   = "governor-e2e-envoy"
	envoyURL    = "http://127.0.0.1:10000"
	envoyReady  = "http://127.0.0.1:9901/ready"
	cpAddr      = "127.0.0.1:50051"
	dpAddr      = "0.0.0.0:50052" // reachable from the Envoy container via host-gateway
	usHost      = "us-central1-aiplatform.googleapis.com"
	euHost      = "europe-west1-aiplatform.googleapis.com"
	usPath      = "/v1/projects/p1/locations/us-central1/publishers/google/models/gemini-3.5-flash:generateContent"
	euPath      = "/v1/projects/p1/locations/europe-west1/publishers/google/models/gemini-3.5-flash:generateContent"
	clientToken = "Bearer test-token"
)

var fakeRegions = map[string]string{"us-central1": usHost, "europe-west1": euHost}

const e2eConfig = `
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

type fakeMetrics struct {
	mu        sync.Mutex
	usRPMUsed int64
}

func (f *fakeMetrics) set(rpm int64) { f.mu.Lock(); f.usRPMUsed = rpm; f.mu.Unlock() }

func (f *fakeMetrics) FetchMetrics(_ context.Context, _, _, _ []string) (map[string]*controlplane.RawModelQuota, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]*controlplane.RawModelQuota{
		"p1/us-central1/gemini-3.5-flash":  {ProjectId: "p1", Region: "us-central1", Model: "gemini-3.5-flash", MaxRpm: 1000, MaxTpm: 1_000_000, CurrentRpm: f.usRPMUsed},
		"p1/europe-west1/gemini-3.5-flash": {ProjectId: "p1", Region: "europe-west1", Model: "gemini-3.5-flash", MaxRpm: 1000, MaxTpm: 1_000_000, CurrentRpm: 50},
	}, nil
}

// echoed is what a fakevertex endpoint reports back.
type echoed struct {
	ServedBy string            `json:"served_by"`
	Path     string            `json:"path"`
	Host     string            `json:"host"`
	Headers  map[string]string `json:"headers"`
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// writeCerts writes a test CA and a server cert for *.aiplatform.googleapis.com-style hosts.
func writeCerts(t *testing.T, dir string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "governor-e2e-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "fakevertex"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames:    []string{usHost, euHost},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srvKeyDER, _ := x509.MarshalECPrivateKey(srvKey)

	write := func(name, typ string, der []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ca.pem", "CERTIFICATE", caDER)
	write("server.pem", "CERTIFICATE", srvDER)
	write("server-key.pem", "EC PRIVATE KEY", srvKeyDER) // test-only key, readable by the container user
}

// writeEnvoyConfig copies the real envoy.yaml with only the environment-specific
// values swapped: governor address, upstream CA bundle, and admin bind address.
func writeEnvoyConfig(t *testing.T, root, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "deploy", "envoy", "envoy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(b)
	for old, repl := range map[string]string{
		"socket_address: { address: 127.0.0.1, port_value: 50052 }": "socket_address: { address: host.docker.internal, port_value: 50052 }",
		"/etc/ssl/certs/ca-certificates.crt":                        "/work/ca.pem",
		"socket_address: { address: 127.0.0.1, port_value: 9901 }":  "socket_address: { address: 0.0.0.0, port_value: 9901 }",
	} {
		if !strings.Contains(cfg, old) {
			t.Fatalf("envoy.yaml no longer contains %q; update the e2e substitutions", old)
		}
		cfg = strings.ReplaceAll(cfg, old, repl)
	}
	if err := os.WriteFile(filepath.Join(dir, "envoy.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cleanupDocker() {
	names := []string{envoyName}
	for region := range fakeRegions {
		names = append(names, "governor-e2e-"+region)
	}
	_ = exec.Command("docker", append([]string{"rm", "-f"}, names...)...).Run()
	_ = exec.Command("docker", "network", "rm", network).Run()
}

func startDockerTopology(t *testing.T) {
	t.Helper()
	root := repoRoot(t)
	work := t.TempDir()
	if err := os.Chmod(work, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCerts(t, work)
	writeEnvoyConfig(t, root, work)

	build := exec.Command("go", "build", "-o", filepath.Join(work, "fakevertex"), "./test/e2e/fakevertex")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fakevertex: %v\n%s", err, out)
	}

	cleanupDocker()
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", envoyName).CombinedOutput()
			t.Logf("envoy logs:\n%s", logs)
		}
		cleanupDocker()
	})
	docker(t, "network", "create", network)

	for region, host := range fakeRegions {
		docker(t, "run", "-d", "--name", "governor-e2e-"+region, "--network", network, "--network-alias", host,
			"-v", work+":/work:ro", "--entrypoint", "/work/fakevertex", envoyImage,
			"--region", region, "--cert", "/work/server.pem", "--key", "/work/server-key.pem")
	}
	docker(t, "run", "-d", "--name", envoyName, "--network", network,
		"--add-host", "host.docker.internal:host-gateway",
		"-p", "127.0.0.1:10000:10000", "-p", "127.0.0.1:9901:9901",
		"-v", work+":/work:ro", envoyImage, "-c", "/work/envoy.yaml", "-l", "warn")

	waitFor(t, "envoy ready", 30*time.Second, func() bool {
		resp, err := http.Get(envoyReady)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

func startGRPC(t *testing.T, addr string, register func(*grpc.Server)) {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	s := grpc.NewServer()
	register(s)
	go s.Serve(lis)
	t.Cleanup(s.Stop)
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type result struct {
	status int
	resp   http.Header
	echo   echoed
}

func call(t *testing.T, host, priority, policy string) result {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, envoyURL+usPath, strings.NewReader(`{"contents":[]}`))
	req.Host = host
	req.Header.Set("Authorization", clientToken)
	req.Header.Set("Content-Type", "application/json")
	if priority != "" {
		req.Header.Set("X-Request-Priority", priority)
	}
	if policy != "" {
		req.Header.Set("X-Governor-Policy", policy)
	}
	// A spoofed governor header that must never reach the upstream.
	req.Header.Set("X-Governor-Target-Region", "spoofed")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	r := result{status: resp.StatusCode, resp: resp.Header}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&r.echo); err != nil {
			t.Fatalf("decode echo: %v", err)
		}
	}
	return r
}

func TestEnvoyE2E(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfgPath := filepath.Join(t.TempDir(), "governor.yaml")
	if err := os.WriteFile(cfgPath, []byte(e2eConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := governor.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Control plane with fake GCP metrics.
	metrics := &fakeMetrics{usRPMUsed: 500}
	store := controlplane.NewInMemoryStore()
	rec, err := controlplane.NewReconciler(cfg, metrics, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	qds, _ := server.NewServer(store)
	startGRPC(t, cpAddr, func(s *grpc.Server) { pb.RegisterQuotaDiscoveryServiceServer(s, qds) })

	// Data plane.
	engine, err := governor.NewCascadeEngine(cfg.CustomPolicies)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(cpAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cache := dataplane.NewSnapshotCache()
	go cache.Run(ctx, pb.NewQuotaDiscoveryServiceClient(conn), "e2e")
	startGRPC(t, dpAddr, func(s *grpc.Server) {
		extprocv3.RegisterExternalProcessorServer(s, dataplane.NewExtProcServer(dataplane.NewEvaluator(cache, engine)))
	})

	usUtil := func() float64 {
		if s := cache.Get(); s != nil && s.ProjectQuotas["p1/us-central1/gemini-3.5-flash"] != nil {
			return s.ProjectQuotas["p1/us-central1/gemini-3.5-flash"].UtilizationRpm
		}
		return -1
	}
	waitFor(t, "initial snapshot", 5*time.Second, func() bool { return usUtil() == 0.5 })

	startDockerTopology(t)

	t.Run("healthy/best_effort served by us-central1", func(t *testing.T) {
		r := call(t, usHost, "best_effort", "")
		expect(t, r.status, http.StatusOK, "status")
		expect(t, r.echo.ServedBy, "us-central1", "served_by")
		expect(t, r.echo.Headers["X-Governor-Decision"], dataplane.DecisionForward, "decision")
		expect(t, r.echo.Headers["Authorization"], clientToken, "authorization passthrough")
		expect(t, r.echo.Headers["X-Governor-Target-Region"], "", "spoofed header stripped")
	})

	t.Run("non vertex host is rejected (no open proxy)", func(t *testing.T) {
		r := call(t, "example.com", "critical", "")
		expect(t, r.status, http.StatusNotFound, "status")
	})

	metrics.set(970)
	if err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "saturated snapshot", 5*time.Second, func() bool { return usUtil() == 0.97 })

	t.Run("saturated/best_effort shed with 429", func(t *testing.T) {
		r := call(t, usHost, "best_effort", "")
		expect(t, r.status, http.StatusTooManyRequests, "status")
		expect(t, r.resp.Get("Retry-After"), "5", "retry-after")
		expect(t, r.resp.Get("X-Governor-Decision"), dataplane.DecisionShed, "decision")
	})

	t.Run("saturated/critical still served by us-central1", func(t *testing.T) {
		r := call(t, usHost, "critical", "")
		expect(t, r.status, http.StatusOK, "status")
		expect(t, r.echo.ServedBy, "us-central1", "served_by")
		expect(t, r.echo.Path, usPath, "path")
	})

	t.Run("saturated/custom rerouted and served by europe-west1", func(t *testing.T) {
		r := call(t, usHost, "custom", "fallback-eu")
		expect(t, r.status, http.StatusOK, "status")
		expect(t, r.echo.ServedBy, "europe-west1", "served_by")
		expect(t, r.echo.Host, euHost, "host")
		expect(t, r.echo.Path, euPath, "path")
		expect(t, r.echo.Headers["X-Governor-Decision"], dataplane.DecisionReroute, "decision")
		expect(t, r.echo.Headers["X-Governor-Original-Path"], usPath, "original path")
		expect(t, r.echo.Headers["Authorization"], clientToken, "authorization passthrough")
	})
}

func expect(t *testing.T, got, want any, what string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}
