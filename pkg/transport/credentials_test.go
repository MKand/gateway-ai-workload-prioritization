package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/controlplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// writeSelfSigned writes a self-signed cert/key for 127.0.0.1 and returns their paths.
func writeSelfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "governor-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestServerCredentialsValidation(t *testing.T) {
	cert, key := writeSelfSigned(t)
	tests := []struct {
		name          string
		cert, key     string
		allowInsecure bool
		wantErr       bool
	}{
		{name: "no tls and not insecure is rejected", wantErr: true},
		{name: "explicit insecure", allowInsecure: true},
		{name: "cert without key", cert: cert, wantErr: true},
		{name: "insecure plus cert is rejected", cert: cert, key: key, allowInsecure: true, wantErr: true},
		{name: "valid tls", cert: cert, key: key},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ServerCredentials(tc.cert, tc.key, tc.allowInsecure)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestClientCredentialsValidation(t *testing.T) {
	cert, _ := writeSelfSigned(t)
	if _, err := ClientCredentials(cert, "", true); err == nil {
		t.Error("insecure plus CA file: want error")
	}
	if _, err := ClientCredentials(filepath.Join(t.TempDir(), "missing.pem"), "", false); err == nil {
		t.Error("missing CA file: want error")
	}
	if _, err := ClientCredentials(cert, "", false); err != nil {
		t.Errorf("valid CA file: %v", err)
	}
}

// TestTLSEndToEnd serves QuotaDiscoveryService over TLS and checks that only a
// client trusting the CA can fetch a snapshot.
func TestTLSEndToEnd(t *testing.T) {
	certFile, keyFile := writeSelfSigned(t)
	serverCreds, err := ServerCredentials(certFile, keyFile, false)
	if err != nil {
		t.Fatal(err)
	}

	store := controlplane.NewInMemoryStore()
	if err := store.Save(context.Background(), &pb.QuotaSnapshot{}); err != nil {
		t.Fatal(err)
	}
	qds, err := server.NewServer(store)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.Creds(serverCreds))
	pb.RegisterQuotaDiscoveryServiceServer(s, qds)
	go s.Serve(lis)
	t.Cleanup(s.Stop)

	trusted, err := ClientCredentials(certFile, "", false)
	if err != nil {
		t.Fatal(err)
	}
	systemRoots, err := ClientCredentials("", "", false)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		creds   credentials.TransportCredentials
		wantErr bool
	}{
		{name: "client trusting CA succeeds", creds: trusted},
		{name: "client with system roots rejects self-signed cert", creds: systemRoots, wantErr: true},
		{name: "plaintext client is rejected", creds: insecure.NewCredentials(), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(tc.creds))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err = pb.NewQuotaDiscoveryServiceClient(conn).GetQuotaSnapshot(ctx, &pb.GetQuotaSnapshotRequest{})
			if (err != nil) != tc.wantErr {
				t.Errorf("GetQuotaSnapshot err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
