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
	"sync/atomic"
	"syscall"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/server"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/telemetry"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func main() {
	configPath := flag.String("config", "config/governor.yaml", "path to governor YAML configuration file")
	port := flag.Int("port", 50051, "gRPC server listening port")
	tlsCert := flag.String("tls-cert", "", "PEM certificate for the gRPC server")
	tlsKey := flag.String("tls-key", "", "PEM private key for the gRPC server")
	allowInsecure := flag.Bool("insecure", false, "serve gRPC without TLS (local development only)")
	once := flag.Bool("once", false, "poll GCP quotas and usage once, print the snapshot as JSON to stdout and exit")
	metricsAddr := flag.String("metrics-addr", ":9091", "address for /metrics and /healthz (empty disables)")
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
		fatal("failed to load config", err)
	}

	store := controlplane.NewInMemoryStore()
	quotaServer, err := server.NewServer(store)
	if err != nil {
		fatal("failed to create quota server", err)
	}

	quotaClient, err := controlplane.NewGCPQuotaRequestClient(ctx)
	if err != nil {
		fatal("failed to create quota request client", err)
	}
	defer quotaClient.Close()

	usageClient, err := controlplane.NewGCPUsageRequestClient(ctx)
	if err != nil {
		fatal("failed to create usage request client", err)
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
		fatal("failed to create GCP client", err)
	}

	reconciler, err := controlplane.NewReconciler(cfg, gcpClient, store)
	if err != nil {
		fatal("failed to create reconciler", err)
	}

	if *once {
		if err := runOnce(ctx, cfg, reconciler, store); err != nil {
			fatal("one-shot reconcile failed", err)
		}
		return
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		fatal("failed to listen", err, "port", *port)
	}

	reg := telemetry.NewRegistry()
	reconciles := reg.NewCounterVec("governor_reconcile_total", "Quota reconcile cycles by result.", "result")
	var lastSuccess atomic.Int64
	reg.NewGaugeFunc("governor_last_reconcile_success_timestamp_seconds",
		"Unix time of the last successful reconcile (0 before the first).",
		func() []telemetry.Sample { return []telemetry.Sample{{Value: float64(lastSuccess.Load())}} })
	reg.NewGaugeFunc("governor_active_dataplane_replicas",
		"Data plane replicas that reported usage within the share TTL.",
		func() []telemetry.Sample {
			return []telemetry.Sample{{Value: float64(quotaServer.ShareTracker().ActiveReplicas())}}
		})

	healthSrv := health.NewServer()
	// The quota service reports NOT_SERVING until the first successful
	// reconcile, so readiness checks wait for real quota data.
	healthSrv.SetServingStatus(pb.QuotaDiscoveryService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
	reconciler.WithObserver(func(err error) {
		if err != nil {
			reconciles.Inc("error")
			return
		}
		reconciles.Inc("success")
		lastSuccess.Store(time.Now().Unix())
		healthSrv.SetServingStatus(pb.QuotaDiscoveryService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	})

	// Bypass subtraction: compare org-wide usage from Cloud Monitoring with
	// what the governor admitted, and shrink every share by the difference.
	bypass := controlplane.NewBypassTracker(cfg, nil)
	quotaServer.WithBypassTracker(bypass)
	bypassReads := reg.NewCounterVec("governor_bypass_reads_total", "Cloud Monitoring usage reads for bypass estimation, by result.", "result")
	reg.NewGaugeFunc("governor_bypass_data_age_seconds",
		"Age of the last Cloud Monitoring usage read (-1 before the first).",
		func() []telemetry.Sample {
			age := bypass.DataAge()
			if age < 0 {
				return []telemetry.Sample{{Value: -1}}
			}
			return []telemetry.Sample{{Value: age.Seconds()}}
		})
	reg.NewGaugeFunc("governor_bypass_estimate",
		"Estimated per-minute traffic reaching Vertex AI without going through the governor.",
		func() []telemetry.Sample {
			var out []telemetry.Sample
			for _, target := range bypass.Targets() {
				e := bypass.Estimate(target)
				out = append(out,
					telemetry.Sample{LabelValues: []string{target, "requests"}, Value: float64(e.BypassRequests)},
					telemetry.Sample{LabelValues: []string{target, "tokens"}, Value: float64(e.BypassTokens)})
			}
			return out
		}, "target", "unit")
	reg.NewGaugeFunc("governor_allowance_factor",
		"Fraction of each allowance left after subtracting bypass traffic and the margin (1 = nothing subtracted).",
		func() []telemetry.Sample {
			var out []telemetry.Sample
			for _, target := range bypass.Targets() {
				e := bypass.Estimate(target)
				out = append(out,
					telemetry.Sample{LabelValues: []string{target, "rpm"}, Value: e.RPMFactor},
					telemetry.Sample{LabelValues: []string{target, "tpm"}, Value: e.TPMFactor})
			}
			return out
		}, "target", "unit")
	go controlplane.RunBypassLoop(ctx, func(ctx context.Context) (controlplane.UsageSeries, error) {
		return gcpClient.FetchUsageSeries(ctx, cfg.MetricsScopeProject, cfg.ProjectIDs, cfg.Regions, cfg.Models)
	}, bypass, cfg.PollInterval, func(err error) {
		if err != nil {
			bypassReads.Inc("error")
			return
		}
		bypassReads.Inc("success")
	})

	go func() {
		if err := telemetry.ServeHTTP(ctx, *metricsAddr, reg); err != nil {
			slog.Error("metrics server stopped", "err", err)
		}
	}()

	go func() {
		slog.Info("starting quota reconciler", "poll_interval", cfg.PollInterval)
		if err := reconciler.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("reconciler stopped", "err", err)
		}
	}()

	creds, err := transport.ServerCredentials(*tlsCert, *tlsKey, *allowInsecure)
	if err != nil {
		fatal("invalid TLS settings", err)
	}
	if *allowInsecure {
		slog.Warn("serving gRPC without TLS")
	}
	grpcServer := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterQuotaDiscoveryServiceServer(grpcServer, quotaServer)
	healthpb.RegisterHealthServer(grpcServer, healthSrv)

	go func() {
		<-ctx.Done()
		slog.Info("shutdown signal received, stopping control plane")
		healthSrv.Shutdown()
		grpcServer.GracefulStop()
	}()

	slog.Info("control plane listening", "port", *port, "metrics_addr", *metricsAddr)
	if err := grpcServer.Serve(lis); err != nil {
		fatal("failed to serve", err)
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

func fatal(msg string, err error, args ...any) {
	slog.Error(msg, append(args, "err", err)...)
	os.Exit(1)
}
