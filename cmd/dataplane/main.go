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
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/logging"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/transport"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
)

func main() {
	configPath := flag.String("config", "config/governor.yaml", "path to governor YAML configuration file (custom cascade policies)")
	port := flag.Int("port", 50052, "ext_proc gRPC listening port")
	controlPlane := flag.String("control-plane", "localhost:50051", "address of the Control Plane QuotaDiscoveryService")
	cpCA := flag.String("control-plane-ca", "", "PEM CA bundle used to verify the control plane's TLS certificate (default: system roots)")
	cpServerName := flag.String("control-plane-server-name", "", "override the server name checked in the control plane's TLS certificate")
	allowInsecure := flag.Bool("insecure", false, "connect to the control plane without TLS (local development only)")
	logLevel := flag.String("log-level", "info", "structured log level (debug, info, warn, error)")
	logFormat := flag.String("log-format", "json", "structured log format (json, text)")
	flag.Parse()

	logger, err := logging.New(*logFormat, *logLevel, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid logging configuration: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := governor.LoadConfig(*configPath)
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "err", err)
		os.Exit(1)
	}

	engine, err := governor.NewCascadeEngineFromConfig(cfg)
	if err != nil {
		logger.Error("failed to build cascade engine", "err", err)
		os.Exit(1)
	}

	creds, err := transport.ClientCredentials(*cpCA, *cpServerName, *allowInsecure)
	if err != nil {
		logger.Error("invalid control plane TLS settings", "err", err)
		os.Exit(1)
	}
	if *allowInsecure {
		logger.Warn("connecting to control plane without TLS; intended for local development only", "control_plane", *controlPlane)
	}
	conn, err := grpc.NewClient(*controlPlane, grpc.WithTransportCredentials(creds))
	if err != nil {
		logger.Error("failed to create control plane client", "control_plane", *controlPlane, "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	hostname, _ := os.Hostname()
	cache := dataplane.NewSnapshotCache()
	cache.SetLogger(logger)
	go func() {
		if err := cache.Run(ctx, pb.NewQuotaDiscoveryServiceClient(conn), hostname); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("quota stream stopped", "err", err)
		}
	}()

	// Each replica needs a unique ID so the control plane can split the
	// allowance by demand; the PID guards against two replicas on one host.
	replicaID := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	reporter := dataplane.NewUsageReporter(pb.NewQuotaDiscoveryServiceClient(conn), engine.Limiter(), replicaID, 0)
	reporter.SetLogger(logger)
	go func() {
		if err := reporter.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("usage reporter stopped", "err", err)
		}
	}()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		logger.Error("failed to listen", "port", *port, "err", err)
		os.Exit(1)
	}

	extProcSrv := dataplane.NewExtProcServer(dataplane.NewEvaluator(cache, engine).WithDefaultTokens(cfg.RequestTokens()))
	extProcSrv.SetLogger(logger)

	grpcServer := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(grpcServer, extProcSrv)

	go func() {
		<-ctx.Done()
		logger.Info("shutdown signal received; stopping data plane ext_proc server")
		grpcServer.GracefulStop()
	}()

	logger.Info("data plane ext_proc listening", "port", *port, "control_plane", *controlPlane)
	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("gRPC server failed", "err", err)
		os.Exit(1)
	}
}
