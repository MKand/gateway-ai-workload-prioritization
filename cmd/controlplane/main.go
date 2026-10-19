package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
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
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := governor.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", *configPath, err)
	}

	store := controlplane.NewInMemoryStore()
	quotaServer, err := server.NewServer(store)
	if err != nil {
		log.Fatalf("Failed to create quota server: %v", err)
	}

	quotaClient, err := controlplane.NewGCPQuotaRequestClient(ctx)
	if err != nil {
		log.Fatalf("Failed to create quota request client: %v", err)
	}
	defer quotaClient.Close()

	usageClient, err := controlplane.NewGCPUsageRequestClient(ctx)
	if err != nil {
		log.Fatalf("Failed to create usage request client: %v", err)
	}
	defer usageClient.Close()

	gcpClient, err := controlplane.NewGCPClient(
		ctx,
		cfg.OrgId,
		cfg.DefaultProjectLimits,
		cfg.DefaultOrgLimits,
		quotaClient,
		usageClient,
	)
	if err != nil {
		log.Fatalf("Failed to create GCP client: %v", err)
	}

	reconciler, err := controlplane.NewReconciler(cfg, gcpClient, store)
	if err != nil {
		log.Fatalf("Failed to create reconciler: %v", err)
	}

	if *once {
		if err := runOnce(ctx, cfg, reconciler, store); err != nil {
			log.Fatalf("One-shot reconcile failed: %v", err)
		}
		return
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", *port, err)
	}

	go func() {
		log.Printf("Starting quota reconciler (poll interval: %v)...", cfg.PollInterval)
		if err := reconciler.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Reconciler stopped with error: %v", err)
		}
	}()

	creds, err := transport.ServerCredentials(*tlsCert, *tlsKey, *allowInsecure)
	if err != nil {
		log.Fatalf("Invalid TLS settings: %v", err)
	}
	if *allowInsecure {
		log.Printf("WARNING: serving gRPC without TLS")
	}
	grpcServer := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterQuotaDiscoveryServiceServer(grpcServer, quotaServer)

	go func() {
		<-ctx.Done()
		log.Println("Shutdown signal received, stopping Control Plane gRPC server...")
		grpcServer.GracefulStop()
	}()

	log.Printf("Gemini Quota Governor Control Plane listening on port %d...", *port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
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
