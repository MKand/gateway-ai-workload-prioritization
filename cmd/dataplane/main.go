package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/dataplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/telemetry"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/transport"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	configPath := flag.String("config", "config/governor.yaml", "path to governor YAML configuration file")
	port := flag.Int("port", 50052, "ext_proc gRPC listening port")
	controlPlane := flag.String("control-plane", "localhost:50051", "address of the Control Plane QuotaDiscoveryService")
	cpCA := flag.String("control-plane-ca", "", "PEM CA bundle used to verify the control plane's TLS certificate (default: system roots)")
	cpServerName := flag.String("control-plane-server-name", "", "override the server name checked in the control plane's TLS certificate")
	allowInsecure := flag.Bool("insecure", false, "connect to the control plane without TLS (local development only)")
	metricsAddr := flag.String("metrics-addr", ":9090", "address for /metrics and /healthz (empty disables)")
	logFormat := flag.String("log-format", "text", "log format: text or json (json uses Cloud Logging severity/message keys)")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	flag.Parse()

	if err := telemetry.SetupLogging(os.Stderr, *logFormat, *logLevel); err != nil {
		fatal("invalid logging flags", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := governor.LoadConfig(*configPath)
	if err != nil {
		fatal("failed to load config", err, "path", *configPath)
	}

	engine, err := governor.NewCascadeEngineFromConfig(cfg)
	if err != nil {
		fatal("failed to build cascade engine", err)
	}

	creds, err := transport.ClientCredentials(*cpCA, *cpServerName, *allowInsecure)
	if err != nil {
		fatal("invalid control plane TLS settings", err)
	}
	if *allowInsecure {
		slog.Warn("connecting to the control plane without TLS", "control_plane", *controlPlane)
	}
	conn, err := grpc.NewClient(*controlPlane, grpc.WithTransportCredentials(creds))
	if err != nil {
		fatal("failed to create control plane client", err, "control_plane", *controlPlane)
	}
	defer conn.Close()
	cpClient := pb.NewQuotaDiscoveryServiceClient(conn)

	hostname, _ := os.Hostname()
	cache := dataplane.NewSnapshotCache()
	go func() {
		if err := cache.Run(ctx, cpClient, hostname); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("quota stream stopped", "err", err)
		}
	}()

	// Each replica needs a unique ID so the control plane can split the
	// allowance by demand; the PID guards against two replicas on one host.
	replicaID := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	reporter := dataplane.NewUsageReporter(cpClient, engine.Limiter(), replicaID, 0)
	go func() {
		if err := reporter.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("usage reporter stopped", "err", err)
		}
	}()

	reg := telemetry.NewRegistry()
	metrics := dataplane.NewMetrics(reg, cache, engine.Limiter())
	go func() {
		if err := telemetry.ServeHTTP(ctx, *metricsAddr, reg); err != nil {
			slog.Error("metrics server stopped", "err", err)
		}
	}()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		fatal("failed to listen", err, "port", *port)
	}

	grpcServer := grpc.NewServer()
	evaluator := dataplane.NewEvaluator(cache, engine).WithDefaultTokens(cfg.RequestTokens())
	extprocv3.RegisterExternalProcessorServer(grpcServer, dataplane.NewExtProcServer(evaluator).WithMetrics(metrics))

	// The data plane is always SERVING: without a snapshot it still enforces
	// the configured allowance locally, and Envoy fails open anyway.
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthSrv)

	go func() {
		<-ctx.Done()
		slog.Info("shutdown signal received, stopping data plane")
		healthSrv.Shutdown()
		grpcServer.GracefulStop()
	}()

	slog.Info("data plane (ext_proc) listening", "port", *port, "control_plane", *controlPlane, "metrics_addr", *metricsAddr)
	if err := grpcServer.Serve(lis); err != nil {
		fatal("failed to serve", err)
	}
}

func fatal(msg string, err error, args ...any) {
	slog.Error(msg, append(args, "err", err)...)
	os.Exit(1)
}
