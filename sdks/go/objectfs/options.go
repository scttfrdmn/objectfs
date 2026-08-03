package objectfs

import "github.com/scttfrdmn/objectfs/internal/config"

// Option is a functional option for configuring a Client.
type Option func(*clientOptions)

// clientOptions holds the configuration for a Client.
type clientOptions struct {
	region         string // AWS region; default: "us-east-1"
	endpoint       string // custom endpoint for MinIO / LocalStack; empty = AWS default
	cacheSize      string // memory cache size (e.g. "512MB"); default: "512MB"
	maxConcurrency int    // max concurrent S3 operations; default: 32
	logLevel       string // log verbosity: DEBUG|INFO|WARN|ERROR; default: "INFO"
	metricsAddr    string // Prometheus endpoint address; default: "127.0.0.1:8080"
	healthAddr     string // health endpoint address; default: "127.0.0.1:8081"
	tlsEnabled     bool   // enable TLS; default: false
}

// defaultOptions returns clientOptions pre-filled with sensible defaults.
func defaultOptions() clientOptions {
	return clientOptions{
		region:         "us-east-1",
		cacheSize:      "512MB",
		maxConcurrency: 32,
		logLevel:       "INFO",
		metricsAddr:    config.DefaultMetricsAddr,
		healthAddr:     config.DefaultHealthAddr,
		tlsEnabled:     false,
	}
}

// WithRegion sets the AWS region (e.g. "us-west-2").
func WithRegion(r string) Option {
	return func(o *clientOptions) {
		o.region = r
	}
}

// WithEndpoint sets a custom S3-compatible endpoint URL (e.g. for MinIO or LocalStack).
func WithEndpoint(e string) Option {
	return func(o *clientOptions) {
		o.endpoint = e
	}
}

// WithCacheSize sets the memory cache size using a human-readable string (e.g. "1GB", "512MB").
func WithCacheSize(s string) Option {
	return func(o *clientOptions) {
		o.cacheSize = s
	}
}

// WithMaxConcurrency sets the maximum number of concurrent S3 operations.
func WithMaxConcurrency(n int) Option {
	return func(o *clientOptions) {
		o.maxConcurrency = n
	}
}

// WithLogLevel sets the log verbosity level: "DEBUG", "INFO", "WARN", or "ERROR".
func WithLogLevel(l string) Option {
	return func(o *clientOptions) {
		o.logLevel = l
	}
}

// WithMetricsAddr sets the host:port on which Prometheus metrics are served.
//
// Defaults to loopback. The endpoint has no authentication and reports per-operation counts, error
// rates, sizes and timings, so exposing it beyond the host is an explicit choice: pass "0.0.0.0:8080"
// to make it reachable from elsewhere.
//
// This replaces WithMetricsPort, which could not express an interface — every value of it bound all of
// them. Passing an address that is malformed or has a port outside 1-65535 is an error from Mount,
// naming the field.
func WithMetricsAddr(addr string) Option {
	return func(o *clientOptions) {
		o.metricsAddr = addr
	}
}

// WithHealthAddr sets the host:port on which the health check endpoint is served.
//
// Loopback by default, for the reason [WithMetricsAddr] gives. Replaces WithHealthPort.
func WithHealthAddr(addr string) Option {
	return func(o *clientOptions) {
		o.healthAddr = addr
	}
}

// WithTLS enables TLS for backend connections.
func WithTLS() Option {
	return func(o *clientOptions) {
		o.tlsEnabled = true
	}
}
