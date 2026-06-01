package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/gerrowadat/nomad-botherer/internal/grpcapi"
)

// Injected at build time via -ldflags.
var version = "dev"

type rootConfig struct {
	server  string
	apiKey  string
	timeout time.Duration
	outFmt  string
	useTLS  bool
}

func newRootCmd() *cobra.Command {
	cfg := &rootConfig{}

	root := &cobra.Command{
		Use:           "nbctl",
		Short:         "Query and control a nomad-botherer instance via gRPC",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVarP(&cfg.server, "server", "s",
		envOrDefault("NBCTL_SERVER", "localhost:9090"),
		"gRPC server address (env: NBCTL_SERVER)")
	root.PersistentFlags().StringVarP(&cfg.apiKey, "api-key", "k", "",
		"API key (env: NBCTL_API_KEY)")
	root.PersistentFlags().DurationVar(&cfg.timeout, "timeout", 10*time.Second,
		"per-request timeout")
	root.PersistentFlags().StringVarP(&cfg.outFmt, "output", "o", "text",
		"output format: text or json")
	root.PersistentFlags().BoolVar(&cfg.useTLS, "tls", false,
		"use TLS for the gRPC connection")

	root.AddCommand(
		newDiffsCmd(cfg),
		newStatusCmd(cfg),
		newRefreshCmd(cfg),
		newVersionCmd(cfg),
	)

	return root
}

// dial resolves the API key, opens a gRPC connection, and returns a client
// plus a closer. The caller must call close() when done.
func (cfg *rootConfig) dial() (grpcapi.NomadBothererClient, func(), error) {
	key := cfg.apiKey
	if key == "" {
		key = os.Getenv("NBCTL_API_KEY")
	}
	if key == "" {
		return nil, nil, fmt.Errorf("API key required: set --api-key or NBCTL_API_KEY")
	}

	var tc credentials.TransportCredentials
	if cfg.useTLS {
		tc = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	} else {
		tc = insecure.NewCredentials()
	}

	conn, err := grpc.NewClient(cfg.server,
		grpc.WithTransportCredentials(tc),
		grpc.WithPerRPCCredentials(bearerCreds{key: key}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to %s: %w", cfg.server, err)
	}
	return grpcapi.NewNomadBothererClient(conn), func() { conn.Close() }, nil
}

// bearerCreds injects the API key into every outgoing RPC as a Bearer token.
type bearerCreds struct{ key string }

func (b bearerCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.key}, nil
}

// RequireTransportSecurity returns false so the token can be sent over a
// plaintext connection (expected to be secured at the network layer).
func (b bearerCreds) RequireTransportSecurity() bool { return false }

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
