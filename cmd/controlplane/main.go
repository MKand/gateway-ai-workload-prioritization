package main

import (
	"fmt"
	"log"
	"net"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/server"
	"google.golang.org/grpc"
)

func main() {
	port := 50051
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", port, err)
	}

	store := controlplane.NewInMemoryStore()
	quotaServer, err := server.NewServer(store)
	if err != nil {
		log.Fatalf("Failed to create quota server: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterQuotaDiscoveryServiceServer(grpcServer, quotaServer)

	log.Printf("Gemini Quota Governor Control Plane listening on port %d...", port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
