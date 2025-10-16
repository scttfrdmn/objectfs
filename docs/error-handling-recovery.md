# Error Handling and Recovery Guide

## Overview

ObjectFS provides a comprehensive error handling and automatic recovery system designed for production environments. The system includes structured errors, automatic retry logic, circuit breakers, graceful degradation, and intelligent connection management with automatic reconnection.

## Table of Contents

- [Core Components](#core-components)
- [Recovery Strategies](#recovery-strategies)
- [Connection Management](#connection-management)
- [Usage Examples](#usage-examples)
- [Best Practices](#best-practices)
- [Troubleshooting](#troubleshooting)

## Core Components

### 1. Structured Errors (`pkg/errors`)

ObjectFS uses structured errors with rich metadata:

```go
type ObjectFSError struct {
    Code       ErrorCode           // Structured error code
    Category   ErrorCategory       // Error category
    Message    string              // Human-readable message
    Details    map[string]interface{} // Additional details
    Component  string              // Component name
    Operation  string              // Operation name
    Retryable  bool                // Whether error is retryable
    UserFacing bool                // Whether to show to users
    Cause      error               // Underlying cause
    Stack      string              // Stack trace
}
```

**Error Categories:**
- `CategoryConfiguration` - Configuration errors
- `CategoryConnection` - Connection and network errors
- `CategoryStorage` - Storage backend errors
- `CategoryFilesystem` - Filesystem operation errors
- `CategoryResource` - Resource management errors
- `CategoryState` - State management errors
- `CategoryOperation` - Operation errors
- `CategoryAuth` - Authentication/authorization errors
- `CategoryInternal` - Internal system errors

**Key Features:**
- **Structured Error Codes**: Semantic codes like `ErrCodeConnectionTimeout`
- **Rich Context**: Add context with `WithContext()`, `WithDetail()`, etc.
- **User-Friendly Messages**: Automatic user-facing messages and recommendations
- **Troubleshooting URLs**: Direct links to documentation
- **Error Wrapping**: Compatible with Go's `errors.Is()` and `errors.As()`

### 2. Retry Logic (`pkg/retry`)

Exponential backoff retry with jitter:

```go
type Config struct {
    MaxAttempts  int           // Maximum retry attempts
    InitialDelay time.Duration // Initial delay before retry
    MaxDelay     time.Duration // Maximum delay between retries
    Multiplier   float64       // Backoff multiplier
    Jitter       bool          // Add randomness to prevent thundering herd
}
```

**Features:**
- Configurable backoff strategy
- Context support for cancellation
- Customizable retry conditions
- Statistics tracking
- Callback support for retry events

### 3. Circuit Breaker (`internal/circuit`)

Prevents cascading failures:

```go
type Config struct {
    MaxRequests uint32        // Max requests in half-open state
    Interval    time.Duration // Measurement interval
    Timeout     time.Duration // Open state timeout
}
```

**States:**
- **Closed**: Normal operation, requests pass through
- **Open**: Too many failures, requests immediately fail
- **Half-Open**: Testing if service recovered, limited requests allowed

### 4. Operation Status Tracking (`pkg/status`)

Real-time operation progress and status:

```go
type Operation struct {
    ID        string          // Unique operation ID
    Type      string          // Operation type
    Status    OperationStatus // Current status
    Progress  *Progress       // Progress information
    StartTime time.Time       // When operation started
}
```

**Features:**
- Progress tracking with ETA calculation
- Subscription to operation updates
- Operation history
- System health integration

### 5. Recovery Manager (`pkg/recovery`)

Intelligent error recovery orchestration:

```go
type RecoveryManager struct {
    config   RecoveryConfig
    retryer  *retry.Retryer
    breakers *circuit.Manager
    logger   *StructuredLogger
}
```

## Recovery Strategies

The Recovery Manager supports multiple strategies:

### 1. Retry Strategy

Automatically retries failed operations with exponential backoff:

```go
rm := recovery.NewRecoveryManager(recovery.DefaultRecoveryConfig())

err := rm.Execute(ctx, "storage", "put-object", func() error {
    return s3Client.PutObject(params)
})
```

**Best for:**
- Transient network failures
- Temporary resource unavailability
- Rate limiting errors

### 2. Circuit Breaker Strategy

Protects against cascading failures:

```go
config := recovery.DefaultRecoveryConfig()
config.DefaultStrategy = recovery.StrategyCircuitBreaker
rm := recovery.NewRecoveryManager(config)

result, err := rm.ExecuteWithResult(ctx, "api", "fetch-data", func() (interface{}, error) {
    return apiClient.FetchData()
})
```

**Best for:**
- External service dependencies
- Database connections
- API calls

### 3. Graceful Degradation Strategy

Continues with reduced functionality:

```go
config := recovery.DefaultRecoveryConfig()
config.DefaultStrategy = recovery.StrategyGracefulDegradation
rm := recovery.NewRecoveryManager(config)

// Register fallback function
rm.RegisterFallback("cache", "get", func(ctx context.Context) (interface{}, error) {
    // Return cached data or default value
    return defaultValue, nil
})

result, err := rm.ExecuteWithResult(ctx, "cache", "get", func() (interface{}, error) {
    return cache.Get(key)
})
```

**Best for:**
- Cache failures (fall back to storage)
- Optional features
- Performance optimizations

### 4. Fallback Strategy

Uses alternative implementation on failure:

```go
rm.RegisterFallback("storage", "read", func(ctx context.Context) (interface{}, error) {
    // Use alternative storage backend
    return alternativeStorage.Read(key)
})

data, err := rm.ExecuteWithResult(ctx, "storage", "read", func() (interface{}, error) {
    return primaryStorage.Read(key)
})
```

**Best for:**
- Multi-backend systems
- Feature flags and A/B testing
- Service migration

### 5. Fail-Fast Strategy

Immediately fails without retry:

```go
config := recovery.DefaultRecoveryConfig()
config.DefaultStrategy = recovery.StrategyFailFast
rm := recovery.NewRecoveryManager(config)
```

**Best for:**
- Validation errors
- Invalid configuration
- Unrecoverable errors

## Connection Management

### Automatic Reconnection

The ConnectionManager handles automatic reconnection with health monitoring:

```go
config := recovery.DefaultConnectionConfig()
config.ReconnectDelay = 1 * time.Second
config.MaxReconnectAttempts = 10
config.EnableAutoReconnect = true

factory := func(ctx context.Context) (interface{}, error) {
    return s3.New(session.Must(session.NewSession()))
}

healthCheck := func(ctx context.Context, conn interface{}) error {
    client := conn.(*s3.S3)
    _, err := client.ListBucketsWithContext(ctx, &s3.ListBucketsInput{})
    return err
}

cm := recovery.NewConnectionManager("s3-client", config, factory, healthCheck)

// Connect
if err := cm.Connect(context.Background()); err != nil {
    log.Fatal(err)
}

// Get connection
conn, err := cm.GetConnection()
if err != nil {
    log.Fatal(err)
}

s3Client := conn.(*s3.S3)
```

**Features:**
- Exponential backoff reconnection
- Periodic health checks
- Automatic recovery from failures
- Connection statistics
- Graceful shutdown

### Connection States

1. **Disconnected**: No active connection
2. **Connecting**: Connection attempt in progress
3. **Connected**: Active, healthy connection
4. **Reconnecting**: Automatic reconnection in progress
5. **Failed**: Exceeded max attempts, manual intervention required

### Connection Pool

For load balancing across multiple connections:

```go
pool := recovery.NewConnectionPool("s3-pool", 5, config, factory, healthCheck)

// Connect all
if err := pool.ConnectAll(context.Background()); err != nil {
    log.Fatal(err)
}

// Get connection (round-robin)
conn, err := pool.GetConnection()
```

## Usage Examples

### Example 1: S3 Operations with Automatic Retry

```go
rm := recovery.NewRecoveryManager(recovery.DefaultRecoveryConfig())

err := rm.Execute(ctx, "s3", "put-object", func() error {
    _, err := s3Client.PutObject(&s3.PutObjectInput{
        Bucket: aws.String("my-bucket"),
        Key:    aws.String("my-key"),
        Body:   bytes.NewReader(data),
    })
    return err
})

if err != nil {
    if objErr, ok := err.(*errors.ObjectFSError); ok {
        log.Printf("Error: %s\n", objErr.UserFacingMessage())
        log.Printf("Recommendation: %s\n", objErr.GetRecommendation())
        log.Printf("Troubleshooting: %s\n", objErr.GetTroubleshootingURL())
    }
}
```

### Example 2: Circuit Breaker for External API

```go
config := recovery.DefaultRecoveryConfig()
config.DefaultStrategy = recovery.StrategyCircuitBreaker
config.CircuitBreakerConfig.MaxRequests = 5
config.CircuitBreakerConfig.Interval = 30 * time.Second
config.CircuitBreakerConfig.Timeout = 60 * time.Second

rm := recovery.NewRecoveryManager(config)

result, err := rm.ExecuteWithResult(ctx, "external-api", "fetch", func() (interface{}, error) {
    resp, err := http.Get("https://api.example.com/data")
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    var data map[string]interface{}
    if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
        return nil, err
    }

    return data, nil
})
```

### Example 3: Graceful Degradation with Cache

```go
config := recovery.DefaultRecoveryConfig()
config.DefaultStrategy = recovery.StrategyGracefulDegradation
rm := recovery.NewRecoveryManager(config)

// Register fallback to fetch from storage
rm.RegisterFallback("cache", "get", func(ctx context.Context) (interface{}, error) {
    log.Println("Cache unavailable, fetching from storage")
    return storage.Get(ctx, key)
})

// Try cache first, fall back to storage automatically
data, err := rm.ExecuteWithResult(ctx, "cache", "get", func() (interface{}, error) {
    return cache.Get(key)
})
```

### Example 4: S3 Connection with Auto-Reconnect

```go
config := recovery.DefaultConnectionConfig()
config.ReconnectDelay = 1 * time.Second
config.MaxReconnectDelay = 30 * time.Second
config.MaxReconnectAttempts = 10
config.HealthCheckInterval = 30 * time.Second

factory := func(ctx context.Context) (interface{}, error) {
    sess := session.Must(session.NewSession(&aws.Config{
        Region: aws.String("us-east-1"),
    }))
    return s3.New(sess), nil
}

healthCheck := func(ctx context.Context, conn interface{}) error {
    client := conn.(*s3.S3)
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    _, err := client.ListBucketsWithContext(ctx, &s3.ListBucketsInput{})
    return err
}

cm := recovery.NewConnectionManager("s3-client", config, factory, healthCheck)

// Connect with timeout
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := cm.Connect(ctx); err != nil {
    log.Fatalf("Failed to connect: %v", err)
}

// Wait for connection to be ready
if err := cm.Wait(ctx); err != nil {
    log.Fatalf("Connection not ready: %v", err)
}

// Get connection and use it
conn, err := cm.GetConnection()
if err != nil {
    log.Fatalf("Failed to get connection: %v", err)
}

s3Client := conn.(*s3.S3)

// Connection is automatically monitored and reconnected if health check fails
```

### Example 5: Monitoring and Statistics

```go
rm := recovery.NewRecoveryManager(recovery.DefaultRecoveryConfig())

// Execute operations...

// Get recovery statistics
stats := rm.GetRecoveryStats()
fmt.Printf("Degraded components: %d\n", stats.DegradedComponents)
fmt.Printf("Active recoveries: %d\n", stats.ActiveRecoveries)
fmt.Printf("Total attempts: %d\n", stats.TotalAttempts)

// Get circuit breaker stats
for name, cbStats := range stats.CircuitBreakers {
    fmt.Printf("Circuit breaker %s: state=%v, requests=%d, failures=%d\n",
        name, cbStats.State, cbStats.Counts.Requests, cbStats.Counts.TotalFailures)
}

// Get degraded components
degraded := rm.GetDegradedComponents()
for component, state := range degraded {
    fmt.Printf("Component %s degraded since %v: %s\n",
        component, state.Since, state.Reason)
}

// Manually recover a component
if err := rm.RecoverComponent("s3"); err != nil {
    log.Printf("Failed to recover component: %v", err)
}
```

## Best Practices

### 1. Choose the Right Strategy

- **Retry**: For transient failures (network timeouts, rate limits)
- **Circuit Breaker**: For protecting against cascading failures
- **Graceful Degradation**: For non-critical features
- **Fallback**: For multi-backend systems
- **Fail-Fast**: For validation and configuration errors

### 2. Configure Appropriate Timeouts

```go
config := recovery.DefaultRecoveryConfig()
config.RetryConfig.InitialDelay = 100 * time.Millisecond
config.RetryConfig.MaxDelay = 30 * time.Second
config.RetryConfig.MaxAttempts = 5
```

### 3. Use Structured Errors

Always wrap errors with structured error information:

```go
if err != nil {
    return errors.NewError(errors.ErrCodeConnectionTimeout, "S3 connection timed out").
        WithComponent("s3-client").
        WithOperation("PutObject").
        WithContext("bucket", bucketName).
        WithContext("key", objectKey).
        WithCause(err).
        WithStack()
}
```

### 4. Implement Health Checks

For ConnectionManager, implement robust health checks:

```go
healthCheck := func(ctx context.Context, conn interface{}) error {
    client := conn.(*s3.S3)

    // Quick operation to verify connectivity
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    _, err := client.HeadBucketWithContext(ctx, &s3.HeadBucketInput{
        Bucket: aws.String("my-bucket"),
    })

    return err
}
```

### 5. Monitor and Alert

```go
// Periodically check recovery stats
ticker := time.NewTicker(1 * time.Minute)
defer ticker.Stop()

for range ticker.C {
    stats := rm.GetRecoveryStats()

    if stats.DegradedComponents > 0 {
        alert.Send("Components degraded", stats)
    }

    for name, cb := range stats.CircuitBreakers {
        if cb.State == circuit.StateOpen {
            alert.Send(fmt.Sprintf("Circuit breaker %s is open", name), cb)
        }
    }
}
```

### 6. Use Context for Cancellation

Always pass and respect context:

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

err := rm.Execute(ctx, "component", "operation", func() error {
    // Check context
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }

    // Perform operation...
    return nil
})
```

### 7. Register Fallbacks Early

Register fallback functions during initialization:

```go
func InitializeRecovery() *recovery.RecoveryManager {
    rm := recovery.NewRecoveryManager(recovery.DefaultRecoveryConfig())

    // Register fallbacks
    rm.RegisterFallback("cache", "get", cacheGetFallback)
    rm.RegisterFallback("storage", "read", storageReadFallback)

    return rm
}
```

## Troubleshooting

### Problem: Operations Keep Failing

**Check:**
1. Error codes and messages
2. Recovery statistics
3. Circuit breaker states

```go
stats := rm.GetRecoveryStats()
fmt.Printf("Circuit breakers: %+v\n", stats.CircuitBreakers)

degraded := rm.GetDegradedComponents()
for component, state := range degraded {
    fmt.Printf("Degraded: %s - %s\n", component, state.Reason)
}
```

**Solution:**
- Check if circuit breaker is open
- Verify network connectivity
- Check AWS credentials
- Review error logs

### Problem: Connection Won't Reconnect

**Check connection state:**

```go
stats := cm.GetStats()
fmt.Printf("State: %v\n", stats.State)
fmt.Printf("Reconnect attempt: %d\n", stats.ReconnectAttempt)
fmt.Printf("Last error: %s\n", stats.LastError)
```

**Solution:**
- Check if max attempts exceeded
- Verify health check is correct
- Check network connectivity
- Manually trigger reconnection:

```go
if err := cm.Reconnect(context.Background()); err != nil {
    log.Printf("Manual reconnection failed: %v", err)
}
```

### Problem: Too Many Retries

**Adjust retry configuration:**

```go
config := recovery.DefaultRecoveryConfig()
config.RetryConfig.MaxAttempts = 3           // Reduce max attempts
config.RetryConfig.InitialDelay = 500 * time.Millisecond
config.RetryConfig.MaxDelay = 10 * time.Second
```

### Problem: Circuit Breaker Opens Too Quickly

**Adjust circuit breaker thresholds:**

```go
config.CircuitBreakerConfig.MaxRequests = 10  // Increase
config.CircuitBreakerConfig.Interval = 60 * time.Second  // Longer interval
config.CircuitBreakerConfig.Timeout = 120 * time.Second  // Longer timeout
```

### Debugging Tips

1. **Enable debug logging:**

```go
loggerConfig := utils.DefaultStructuredLoggerConfig()
loggerConfig.Level = utils.DEBUG
logger, _ := utils.NewStructuredLogger(loggerConfig)

config := recovery.DefaultRecoveryConfig()
config.Logger = logger
```

2. **Add retry callbacks:**

```go
config.RetryConfig.OnRetry = func(attempt int, err error, delay time.Duration) {
    log.Printf("Retry attempt %d after %v: %v", attempt, delay, err)
}
```

3. **Monitor circuit breaker state changes:**

```go
config.CircuitBreakerConfig.OnStateChange = func(name string, from circuit.State, to circuit.State) {
    log.Printf("Circuit breaker %s: %v -> %v", name, from, to)
}
```

## Performance Considerations

### Memory Usage

- Recovery Manager: ~100 KB per instance
- Connection Manager: ~50 KB per connection
- Connection Pool: ~50 KB per connection × pool size

### CPU Impact

- Retry logic: Negligible (<1% CPU)
- Circuit breaker: Negligible (<1% CPU)
- Health checks: ~0.1% CPU per check
- Connection management: ~0.5% CPU per connection

### Recommendations

1. **Use connection pools** for high-throughput scenarios
2. **Tune health check intervals** based on workload
3. **Set appropriate max attempts** to avoid excessive retries
4. **Monitor degraded components** and recover promptly
5. **Use fail-fast for validation** to avoid wasting resources

## API Reference

See package documentation for complete API reference:

- `pkg/errors` - Structured error system
- `pkg/retry` - Retry logic with backoff
- `pkg/status` - Operation status tracking
- `internal/circuit` - Circuit breaker pattern
- `pkg/recovery` - Recovery manager and connection management

## Further Reading

- [Reliability Patterns](https://docs.microsoft.com/en-us/azure/architecture/patterns/category/resiliency)
- [Circuit Breaker Pattern](https://martinfowler.com/bliki/CircuitBreaker.html)
- [Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/)
- [Error Handling Best Practices](https://dave.cheney.net/2016/04/27/dont-just-check-errors-handle-them-gracefully)
