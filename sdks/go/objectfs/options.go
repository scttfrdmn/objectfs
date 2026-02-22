package objectfs

// Option is a functional option for configuring a Client.
type Option func(*clientOptions)

// clientOptions holds the configuration for a Client.
type clientOptions struct {
	region         string // AWS region; default: "us-east-1"
	endpoint       string // custom endpoint for MinIO / LocalStack; empty = AWS default
	cacheSize      string // memory cache size (e.g. "512MB"); default: "512MB"
	maxConcurrency int    // max concurrent S3 operations; default: 32
	logLevel       string // log verbosity: DEBUG|INFO|WARN|ERROR; default: "INFO"
	metricsPort    int    // Prometheus metrics port; default: 8080
	healthPort     int    // health check port; default: 8081
	tlsEnabled     bool   // enable TLS; default: false
}

// defaultOptions returns clientOptions pre-filled with sensible defaults.
func defaultOptions() clientOptions {
	return clientOptions{
		region:         "us-east-1",
		cacheSize:      "512MB",
		maxConcurrency: 32,
		logLevel:       "INFO",
		metricsPort:    8080,
		healthPort:     8081,
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

// WithMetricsPort sets the port on which Prometheus metrics are served.
func WithMetricsPort(p int) Option {
	return func(o *clientOptions) {
		o.metricsPort = p
	}
}

// WithHealthPort sets the port on which the health check endpoint is served.
func WithHealthPort(p int) Option {
	return func(o *clientOptions) {
		o.healthPort = p
	}
}

// WithTLS enables TLS for backend connections.
func WithTLS() Option {
	return func(o *clientOptions) {
		o.tlsEnabled = true
	}
}
