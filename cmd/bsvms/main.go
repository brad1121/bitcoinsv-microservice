package main

import (
	"context"
	"encoding/base64"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	bsv "github.com/brad1121/bitcoinsv-sdk-go/sdk"
	bsvmspb "github.com/brad1121/bsvms/gen/bsvms/v1"
	"github.com/brad1121/bsvms/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func main() {
	addr := flag.String("addr", env("BSVMS_ADDR", ":50051"), "gRPC listen address")
	dataDir := flag.String("data-dir", env("BSVMS_DATA_DIR", "data"), "state directory")
	networkName := flag.String("network", env("BSVMS_NETWORK", "mainnet"), "mainnet, testnet, stn, or regtest")
	connect := flag.Bool("connect", envBool("BSVMS_CONNECT", true), "connect to BSV P2P network")
	authEnabled := flag.Bool("auth", envBool("BSVMS_AUTH", false), "require JWT auth")
	enableCustomSpend := flag.Bool("enable-custom-spend", envBool("BSVMS_ENABLE_CUSTOM_SPEND", false), "enable BroadcastCustomSpend RPC")
	jwtSecret := flag.String("jwt-secret", env("BSVMS_JWT_SECRET", ""), "JWT HS256 secret, raw or base64")
	dataKey := flag.String("data-key", env("BSVMS_DATA_KEY", ""), "32-byte AES-GCM data key, raw or base64")
	flag.Parse()

	absDataDir, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("resolve data dir: %v", err)
	}
	if err := os.MkdirAll(absDataDir, 0o700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	node := bsv.New(network(*networkName))
	if *connect {
		if err := node.Connect(); err != nil {
			log.Fatalf("connect BSV node: %v", err)
		}
		defer node.Disconnect()
	}

	svc, err := service.NewWithOptions(context.Background(), node, absDataDir, service.Options{
		AuthEnabled:       *authEnabled,
		EnableCustomSpend: *enableCustomSpend,
		JWTSecret:         secretBytes(*jwtSecret),
		DataKey:           secretBytes(*dataKey),
	})
	if err != nil {
		log.Fatalf("init service: %v", err)
	}
	defer svc.Close()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	var serverOpts []grpc.ServerOption
	if svc.AuthEnabled() {
		serverOpts = append(serverOpts, grpc.UnaryInterceptor(svc.UnaryAuthInterceptor()), grpc.StreamInterceptor(svc.StreamAuthInterceptor()))
	}
	server := grpc.NewServer(serverOpts...)
	bsvmspb.RegisterBSVMSServer(server, svc)
	reflection.Register(server)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()

	log.Printf("bsvms listening on %s network=%s data_dir=%s", *addr, node.Network(), absDataDir)
	if err := server.Serve(lis); err != nil {
		log.Fatalf("serve grpc: %v", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(os.Getenv(key))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func secretBytes(value string) []byte {
	if value == "" {
		return nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return decoded
	}
	return []byte(value)
}

func network(name string) bsv.NetworkType {
	switch strings.ToLower(name) {
	case "testnet":
		return bsv.Testnet
	case "stn":
		return bsv.STN
	case "regtest":
		return bsv.Regtest
	default:
		return bsv.Mainnet
	}
}
