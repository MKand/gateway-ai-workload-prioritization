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
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/logging"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/server"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/transport"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

func main() {
	configPath := flag.String("config", "config/governor.yaml", "path to governor YAML configuration file")
	port := flag.Int("port", 50051, "gRPC server listening port")
	tlsCert := flag.String("tls-cert", "", "PEM certificate for the gRPC server")
	tlsKey := flag.String("tls-key", "", "PEM private key for the gRPC server")
	allowInsecure := flag.Bool("insecure", false, "serve gRPC without TLS (local development only)")
	once := flag.Bool("once", false, "poll GCP quotas and usage once, print the snapshot as JSON to stdout and exit")
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

	store := controlplane.NewInMemoryStore()
	quotaServer, err := server.NewServer(store)
	if err != nil {
		logger.Error("failed to create quota server", "err", err)
		os.Exit(1)
	}

	quotaClient, err := controlplane.NewGCPQuotaRequestClient(ctx)
	if err != nil {
		logger.Error("failed to create quota request client", "err", err)
		os.Exit(1)
	}
	defer quotaClient.Close()

	usageClient, err := controlplane.NewGCPUsageRequestClient(ctx)
	if err != nil {
		logger.Error("failed to create usage request client", "err", err)
		os.Exit(1)
	}
	defer usageClient.Close()

	gcpClient, err := controlplane.NewGCPClient(
		ctx,
		cfg.DefaultProjectLimits,
		cfg.Allowances,
		quotaClient,
		usageClient,
	)
	if err != nil {
		logger.Error("failed to create GCP client", "err", err)
		os.Exit(1)
	}

	reconciler, err := controlplane.NewReconciler(cfg, gcpClient, store)
	if err != nil {
		logger.Error("failed to create reconciler", "err", err)
		os.Exit(1)
	}
	reconciler.SetLogger(logger)

	if *once {
		if err := runOnce(ctx, cfg, reconciler, store); err != nil {
			logger.Error("one-shot reconcile failed", "err", err)
			os.Exit(1)
		}
		return
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		logger.Error("failed to listen", "port", *port, "err", err)
		os.Exit(1)
	}

	go func() {
		logger.Info("starting quota reconciler", "poll_interval", cfg.PollInterval)
		if err := reconciler.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("reconciler stopped with error", "err", err)
		}
	}()

	creds, err := transport.ServerCredentials(*tlsCert, *tlsKey, *allowInsecure)
	if err != nil {
		logger.Error("invalid TLS settings", "err", err)
		os.Exit(1)
	}
	if *allowInsecure {
		logger.Warn("serving gRPC without TLS; intended for local development only")
	}
	grpcServer := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterQuotaDiscoveryServiceServer(grpcServer, quotaServer)

	go func() {
		<-ctx.Done()
		logger.Info("shutdown signal received; stopping control plane gRPC server")
		grpcServer.GracefulStop()
	}()

	logger.Info("control plane listening", "port", *port)
	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("gRPC server failed", "err", err)
		os.Exit(1)
	}
}

// runOnce performs a single reconcile against live GCP APIs and prints the
// resulting QuotaSnapshot. It is the manual validation path for quota/usage reads.
func runOnce(ctx context.Context, cfg *governor.Config, r *controlplane.Reconciler, store controlplane.SnapshotStore) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.PollTimeout+5*time.Second)
	defer cancel()

	if err := r.ReconcileOnce(ctx); err != nil {
		return err
	}
	snap, err := store.Get(ctx)
	if err != nil {
		return err
	}
	out, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", EmitUnpopulated: true}.Marshal(snap)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
