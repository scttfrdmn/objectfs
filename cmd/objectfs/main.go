package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/objectfs/objectfs/internal/adapter"
	"github.com/objectfs/objectfs/internal/config"
	"github.com/objectfs/objectfs/pkg/utils"
)

const (
	version = "0.7.0"
	banner  = `
 ___  _     _           _   ___ ___
/ _ \| |__ (_) ___  ___| |_| __/ __|
| | | | '_ \| |/ _ \/ __| __|  _\__ \
| |_| | |_) | |  __| (__| |_| | |___/
 \___/|_.__/| |\___|\___|\__|_| |___/
           |_|

Enterprise-Grade High-Performance POSIX Filesystem for Object Storage
Version: %s
`
)

var (
	// Global flags
	showVersion = flag.Bool("version", false, "Show version information")
	showHelp    = flag.Bool("help", false, "Show help information")
	debug       = flag.Bool("debug", false, "Enable debug mode")

	// Mount command flags
	configFile     = flag.String("config", "", "Configuration file path")
	logLevel       = flag.String("log-level", "INFO", "Log level (DEBUG, INFO, WARN, ERROR)")
	dryRun         = flag.Bool("dry-run", false, "Validate configuration without mounting")
	cacheSize      = flag.String("cache-size", "", "Cache size (e.g., 2GB)")
	maxConcurrency = flag.Int("max-concurrency", 0, "Maximum concurrent operations")
)

func init() {
	flag.Usage = func() {
		fmt.Printf(banner, version)
		fmt.Fprintf(os.Stderr, "\nUsage: %s [options] <storage-uri> <mount-point>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Arguments:\n")
		fmt.Fprintf(os.Stderr, "  storage-uri   Object storage URI (e.g., s3://my-bucket)\n")
		fmt.Fprintf(os.Stderr, "  mount-point   Local directory to mount the filesystem\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s s3://my-bucket /mnt/s3\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s --config /etc/objectfs/config.yaml s3://my-bucket /mnt/s3\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s --cache-size 4GB --max-concurrency 200 s3://my-bucket /mnt/s3\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nFor more information, visit: https://github.com/objectfs/objectfs\n")
	}
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("ObjectFS version %s\n", version)
		os.Exit(0)
	}

	if *showHelp {
		flag.Usage()
		os.Exit(0)
	}

	// Validate arguments
	args := flag.Args()
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "Error: Expected exactly 2 arguments (storage-uri and mount-point)\n\n")
		flag.Usage()
		os.Exit(1)
	}

	storageURI := args[0]
	mountPoint := args[1]

	// Validate mount point
	if err := validateMountPoint(mountPoint); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Invalid mount point: %v\n", err)
		os.Exit(1)
	}

	// Load configuration
	cfg, err := loadConfiguration()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Apply command line overrides
	applyCommandLineOverrides(cfg)

	// Dry run mode - validate configuration and exit
	if *dryRun {
		fmt.Printf("Configuration validation successful\n")
		fmt.Printf("Storage URI: %s\n", storageURI)
		fmt.Printf("Mount Point: %s\n", mountPoint)
		fmt.Printf("Cache Size: %s\n", cfg.Performance.CacheSize)
		fmt.Printf("Max Concurrency: %d\n", cfg.Performance.MaxConcurrency)
		os.Exit(0)
	}

	// Set up logging
	if err := utils.SetupLogging(cfg.Global.LogLevel, cfg.Global.LogFile); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to setup logging: %v\n", err)
		os.Exit(1)
	}

	log.Printf("Starting ObjectFS %s", version)
	log.Printf("Storage URI: %s", storageURI)
	log.Printf("Mount Point: %s", mountPoint)

	// Create adapter
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	adapterInstance, err := adapter.New(ctx, storageURI, mountPoint, cfg)
	if err != nil {
		log.Fatalf("Failed to create adapter: %v", err)
	}

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// Start the adapter
	if err := adapterInstance.Start(ctx); err != nil {
		log.Fatalf("Failed to start adapter: %v", err)
	}

	log.Printf("ObjectFS mounted successfully at %s", mountPoint)

	// Wait for signals
	sig := <-sigChan
	log.Printf("Received signal %v, shutting down...", sig)

	// Graceful shutdown
	if err := adapterInstance.Stop(ctx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	log.Printf("ObjectFS shutdown complete")
}

// Command handlers

func validateMountPoint(mountPoint string) error {
	// Check if mount point exists
	info, err := os.Stat(mountPoint)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("mount point does not exist: %s", mountPoint)
		}
		return fmt.Errorf("cannot access mount point: %v", err)
	}

	// Check if it's a directory
	if !info.IsDir() {
		return fmt.Errorf("mount point is not a directory: %s", mountPoint)
	}

	// Check if directory is empty
	// Validate mount point path
	cleanMountPoint := filepath.Clean(mountPoint)
	if strings.Contains(cleanMountPoint, "..") {
		return fmt.Errorf("invalid mount point path: %s", mountPoint)
	}

	f, err := os.Open(cleanMountPoint)
	if err != nil {
		return fmt.Errorf("cannot open mount point: %v", err)
	}
	defer func() { _ = f.Close() }()

	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("cannot read mount point: %v", err)
	}

	if len(names) > 0 {
		return fmt.Errorf("mount point is not empty: %s", mountPoint)
	}

	return nil
}

func loadConfiguration() (*config.Configuration, error) {
	cfg := config.NewDefault()

	if *configFile != "" {
		if err := cfg.LoadFromFile(*configFile); err != nil {
			return nil, fmt.Errorf("failed to load config file: %v", err)
		}
	}

	// Load from environment variables
	if err := cfg.LoadFromEnv(); err != nil {
		return nil, fmt.Errorf("failed to load environment variables: %v", err)
	}

	return cfg, nil
}

func applyCommandLineOverrides(cfg *config.Configuration) {
	if *logLevel != "" {
		cfg.Global.LogLevel = *logLevel
	}

	if *cacheSize != "" {
		cfg.Performance.CacheSize = *cacheSize
	}

	if *maxConcurrency > 0 {
		cfg.Performance.MaxConcurrency = *maxConcurrency
	}

	if *debug {
		cfg.Global.LogLevel = "DEBUG"
	}

	// Apply other overrides as needed
}
