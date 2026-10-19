// Package transport builds gRPC transport credentials for the Control Plane <->
// Data Plane link. TLS is the default; plaintext must be requested explicitly.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ServerCredentials returns TLS credentials loaded from certFile/keyFile.
// If allowInsecure is true and no cert/key is given, plaintext is used.
// Without a cert/key and without allowInsecure, it returns an error so a
// server never silently starts unencrypted.
func ServerCredentials(certFile, keyFile string, allowInsecure bool) (credentials.TransportCredentials, error) {
	if certFile == "" && keyFile == "" {
		if allowInsecure {
			return insecure.NewCredentials(), nil
		}
		return nil, errors.New("TLS cert and key are required (or pass --insecure for local development)")
	}
	if certFile == "" || keyFile == "" {
		return nil, errors.New("both TLS cert and key must be set")
	}
	if allowInsecure {
		return nil, errors.New("--insecure cannot be combined with a TLS cert/key")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}), nil
}

// ClientCredentials returns TLS credentials that verify the server against
// caFile, or against the system roots when caFile is empty. serverName
// overrides the name checked in the server certificate (optional).
// If allowInsecure is true, plaintext is used instead.
func ClientCredentials(caFile, serverName string, allowInsecure bool) (credentials.TransportCredentials, error) {
	if allowInsecure {
		if caFile != "" {
			return nil, errors.New("--insecure cannot be combined with a TLS CA file")
		}
		return insecure.NewCredentials(), nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no valid certificates in CA file %s", caFile)
		}
		cfg.RootCAs = pool
	}
	return credentials.NewTLS(cfg), nil
}
