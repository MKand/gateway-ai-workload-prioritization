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

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/server"
	"google.golang.org/grpc"
)

func main() {
	configPath := flag.String("config", "config/governor.yaml", "path to governor YAML configuration file")
	port := flag.Int("port", 50051, "gRPC server listening port")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := governor.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", *configPath, err)
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", *port, err)
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

	go func() {
		log.Printf("Starting quota reconciler (poll interval: %v)...", cfg.PollInterval)
		if err := reconciler.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Reconciler stopped with error: %v", err)
		}
	}()

	grpcServer := grpc.NewServer()
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
