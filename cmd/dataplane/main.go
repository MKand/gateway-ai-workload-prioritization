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
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/dataplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
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
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := governor.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", *configPath, err)
	}

	engine, err := governor.NewCascadeEngine(cfg.CustomPolicies)
	if err != nil {
		log.Fatalf("Failed to build cascade engine: %v", err)
	}

	creds, err := transport.ClientCredentials(*cpCA, *cpServerName, *allowInsecure)
	if err != nil {
		log.Fatalf("Invalid control plane TLS settings: %v", err)
	}
	if *allowInsecure {
		log.Printf("WARNING: connecting to control plane %s without TLS", *controlPlane)
	}
	conn, err := grpc.NewClient(*controlPlane, grpc.WithTransportCredentials(creds))
	if err != nil {
		log.Fatalf("Failed to create control plane client for %s: %v", *controlPlane, err)
	}
	defer conn.Close()

	hostname, _ := os.Hostname()
	cache := dataplane.NewSnapshotCache()
	go func() {
		if err := cache.Run(ctx, pb.NewQuotaDiscoveryServiceClient(conn), hostname); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Quota stream stopped: %v", err)
		}
	}()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", *port, err)
	}

	grpcServer := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(grpcServer, dataplane.NewExtProcServer(dataplane.NewEvaluator(cache, engine)))

	go func() {
		<-ctx.Done()
		log.Println("Shutdown signal received, stopping Data Plane ext_proc server...")
		grpcServer.GracefulStop()
	}()

	log.Printf("Gemini Quota Governor Data Plane (ext_proc) listening on port %d (control plane: %s)...", *port, *controlPlane)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
