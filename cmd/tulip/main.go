package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/maxmcd/tulip"
)

func main() {
	fmt.Println("🌷")
	// Parse command line flags
	nodeAddr := flag.String("addr", "127.0.0.1", "Node address")
	nodePort := flag.Int("port", 7946, "Node port for Serf")
	httpPort := flag.Int("http", 8080, "HTTP port")
	s3Bucket := flag.String("bucket", "functions", "S3 bucket for function storage")
	s3Endpoint := flag.String("s3-endpoint", "", "S3 endpoint (for non-AWS S3)")
	mountPath := flag.String("mount-path", "/tmp/tulip/mounts", "Path for function mounts")
	baseDir := flag.String("base-dir", "/tmp/tulip", "Base directory for runtime files")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	seeds := flag.String("seeds", "", "Comma-separated list of seed nodes")

	flag.Parse()

	// Parse seeds
	var seedNodes []string
	if *seeds != "" {
		seedNodes = strings.Split(*seeds, ",")
	}

	// Create the config
	config := &tulip.Config{
		NodeAddr:      *nodeAddr,
		NodePort:      *nodePort,
		HTTPPort:      *httpPort,
		S3Bucket:      *s3Bucket,
		S3Endpoint:    *s3Endpoint,
		ClusterSeeds:  seedNodes,
		MountBasePath: *mountPath,
		BaseDir:       *baseDir,
		LogLevel:      *logLevel,
	}

	// Create the server
	ctx := context.Background()
	server, err := tulip.NewTulipServer(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create server: %v\n", err)
		os.Exit(1)
	}

	// Start the server
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start server: %v\n", err)
		os.Exit(1)
	}

	// Handle graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	fmt.Printf("Tulip server started on http://%s:%d\n", *nodeAddr, *httpPort)
	fmt.Println("Press Ctrl+C to exit")

	// Wait for signal
	<-sigs
	fmt.Println("Shutting down...")

	// Stop the server
	if err := server.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "Error during shutdown: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Server stopped")
}
