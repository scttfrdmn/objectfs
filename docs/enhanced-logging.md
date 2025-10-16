# Enhanced Logging Guide

ObjectFS provides a comprehensive structured logging system with support for log rotation, debug tracing, and component-specific log levels.

## Table of Contents

- [Quick Start](#quick-start)
- [Structured Logging](#structured-logging)
- [Log Rotation](#log-rotation)
- [Debug Mode](#debug-mode)
- [Configuration](#configuration)
- [Best Practices](#best-practices)
- [Examples](#examples)

## Quick Start

### Basic Structured Logger

```go
import "github.com/objectfs/objectfs/pkg/utils"

// Create logger with default configuration
config := utils.DefaultStructuredLoggerConfig()
logger, err := utils.NewStructuredLogger(config)
if err != nil {
    log.Fatal(err)
}
defer logger.Close()

// Log messages
logger.Info("Application started")
logger.Error("Failed to connect", map[string]interface{}{
    "host": "localhost",
    "port": 8080,
    "error": err.Error(),
})
```

### With Log Rotation

```go
config := &utils.StructuredLoggerConfig{
    Level:         utils.INFO,
    Format:        utils.FormatJSON,
    IncludeCaller: true,
    Rotation: &utils.RotationConfig{
        Filename:   "/var/log/objectfs/app.log",
        MaxSize:    100, // MB
        MaxAge:     7,   // days
        MaxBackups: 10,
        Compress:   true,
    },
}

logger, err := utils.NewStructuredLogger(config)
if err != nil {
    log.Fatal(err)
}
defer logger.Close()
```

## Structured Logging

### Log Levels

ObjectFS supports six log levels (from least to most severe):

- **TRACE**: Very detailed information for deep debugging
- **DEBUG**: Detailed information for debugging
- **INFO**: General informational messages
- **WARN**: Warning messages
- **ERROR**: Error messages
- **FATAL**: Critical errors that cause program exit

### Logging with Fields

Add structured fields to log messages for better searchability and analysis:

```go
// Single field
logger.Info("User logged in", map[string]interface{}{
    "user_id": 12345,
    "ip": "192.168.1.1",
})

// Using WithField for context
userLogger := logger.WithField("user_id", 12345)
userLogger.Info("Session started")
userLogger.Info("File uploaded")
// Both messages will include user_id=12345

// Using WithFields for multiple context fields
requestLogger := logger.WithFields(map[string]interface{}{
    "request_id": "req-abc-123",
    "user_id": 12345,
    "session": "sess-xyz-789",
})
requestLogger.Info("Processing request")
requestLogger.Debug("Validating input")
```

### Component-Specific Logging

Different components can have different log levels:

```go
logger.SetLevel(utils.INFO) // Global level

// Set DEBUG level only for storage component
logger.SetComponentLevel("storage", utils.DEBUG)

// Create component loggers
storageLogger := logger.WithComponent("storage")
cacheLogger := logger.WithComponent("cache")

// Storage will log debug messages
storageLogger.Debug("Cache miss, fetching from S3") // Logged

// Cache will not log debug messages (uses global INFO level)
cacheLogger.Debug("Evicting expired entries") // Not logged
```

### Output Formats

#### Text Format (Human-Readable)

```go
config.Format = utils.FormatText
```

Output example:
```
2025-10-16 10:30:45.123 [INFO] [app.go:42] User logged in {user_id=12345, ip=192.168.1.1}
```

#### JSON Format (Machine-Parseable)

```go
config.Format = utils.FormatJSON
```

Output example:
```json
{
  "timestamp": "2025-10-16T10:30:45.123Z",
  "level": "INFO",
  "message": "User logged in",
  "fields": {
    "user_id": 12345,
    "ip": "192.168.1.1"
  },
  "caller": "app.go:42"
}
```

### Formatted Messages

Use format string methods for convenience:

```go
logger.Infof("Connected to %s:%d", host, port)
logger.Errorf("Failed to read file %s: %v", filename, err)
logger.Debugf("Cache hit rate: %.2f%%", hitRate * 100)
```

## Log Rotation

### Configuration

```go
rotation := &utils.RotationConfig{
    Filename:   "/var/log/objectfs/app.log",
    MaxSize:    100,  // Maximum size in MB before rotation
    MaxAge:     30,   // Maximum age in days (0 = no age limit)
    MaxBackups: 10,   // Maximum number of old files to keep (0 = keep all)
    Compress:   true, // Compress rotated files with gzip
    LocalTime:  true, // Use local time for backup timestamps
}
```

### Rotation Triggers

Logs are automatically rotated when:
1. File size exceeds `MaxSize` megabytes
2. File age exceeds `MaxAge` days

### Backup File Naming

Rotated files are named with timestamps:
```
app-2025-10-16T10-30-45.log
app-2025-10-16T10-30-45.log.gz  (if compressed)
```

### Manual Rotation

Force rotation programmatically:

```go
if rotator, ok := logger.(*utils.LogRotator); ok {
    err := rotator.ForceRotate()
    if err != nil {
        logger.Error("Failed to rotate logs", map[string]interface{}{
            "error": err.Error(),
        })
    }
}
```

### Cleanup Policy

Old log files are automatically cleaned up based on:
- `MaxBackups`: Keeps only N most recent backup files
- `MaxAge`: Deletes files older than N days

Both policies work together - a file is deleted if it exceeds either limit.

## Debug Mode

Debug mode provides advanced tracing and profiling capabilities for troubleshooting.

### Starting a Debug Session

```go
import "github.com/objectfs/objectfs/pkg/utils"

// Get the debug manager
dm := utils.GetDebugManager()

// Set the logger for debug events
dm.SetLogger(logger)

// Start a debug session
sessionID := "troubleshoot-slow-reads"
components := []string{"storage", "cache"} // Only track these components
maxEvents := 10000 // Maximum events to record

session := dm.StartSession(sessionID, components, maxEvents)
```

### Recording Events

```go
// Manual event recording
dm.RecordEvent("storage", "read", "Reading file from S3", map[string]interface{}{
    "bucket": "my-bucket",
    "key": "/path/to/file.txt",
    "size": 1024,
})

// Using debug traces for operation timing
trace := utils.StartTrace(sessionID, "storage", "write", map[string]interface{}{
    "path": "/test/file.txt",
})

// Perform operation...
time.Sleep(100 * time.Millisecond)

// End trace with success
trace.End("Write completed successfully")

// Or end with error
if err != nil {
    trace.EndWithError(err)
}
```

### Using Context for Automatic Tracing

```go
// Add session ID to context
ctx := utils.WithContext(context.Background(), sessionID)

// Pass context through your call stack
processFile(ctx, "/path/to/file")

// Extract session ID in functions
func processFile(ctx context.Context, path string) error {
    sessionID := utils.FromContext(ctx)
    if sessionID != "" {
        trace := utils.StartTrace(sessionID, "storage", "process_file", map[string]interface{}{
            "path": path,
        })
        defer func() {
            if err != nil {
                trace.EndWithError(err)
            } else {
                trace.End("Processing completed")
            }
        }()
    }

    // ... actual processing ...
}
```

### Analyzing Debug Data

```go
// Get all recorded events
events := session.GetEvents()
for _, event := range events {
    fmt.Printf("[%s] %s.%s: %s (took %v)\n",
        event.Timestamp.Format(time.RFC3339),
        event.Component,
        event.Operation,
        event.Message,
        event.Duration,
    )
}

// Get events for specific component
storageEvents := session.GetEventsByComponent("storage")

// Get session statistics
stats := session.GetStats()
fmt.Printf("Session ID: %s\n", stats["id"])
fmt.Printf("Total events: %d\n", stats["event_count"])
fmt.Printf("Duration: %v\n", stats["duration"])

if eventsByComp, ok := stats["events_by_component"].(map[string]int); ok {
    for comp, count := range eventsByComp {
        fmt.Printf("  %s: %d events\n", comp, count)
    }
}
```

### Capturing Runtime Profiles

```go
// Enable runtime profiling
utils.EnableRuntimeProfiling()
defer utils.DisableRuntimeProfiling()

// Capture profiles during debug session
session.CaptureProfile("goroutine")
session.CaptureProfile("heap")
session.CaptureProfile("block")
session.CaptureProfile("mutex")

// Retrieve profile data
goroutineProfile := session.GetProfile("goroutine")
// Write to file or analyze
os.WriteFile("goroutine.prof", goroutineProfile, 0644)
```

### Stopping Debug Sessions

```go
// Stop session and get final results
finalSession := dm.StopSession(sessionID)

// Session data remains available after stopping
events := finalSession.GetEvents()
stats := finalSession.GetStats()
```

## Configuration

### Environment-Based Configuration

```go
import "os"

func setupLogger() (*utils.StructuredLogger, error) {
    levelStr := os.Getenv("LOG_LEVEL")
    if levelStr == "" {
        levelStr = "INFO"
    }

    level, err := utils.ParseLogLevel(levelStr)
    if err != nil {
        level = utils.INFO
    }

    format := utils.FormatText
    if os.Getenv("LOG_FORMAT") == "json" {
        format = utils.FormatJSON
    }

    config := &utils.StructuredLoggerConfig{
        Level:         level,
        Format:        format,
        IncludeCaller: os.Getenv("LOG_CALLER") == "true",
        IncludeStack:  os.Getenv("LOG_STACK") == "true",
    }

    // Add rotation for file logging
    if logFile := os.Getenv("LOG_FILE"); logFile != "" {
        config.Rotation = &utils.RotationConfig{
            Filename:   logFile,
            MaxSize:    100,
            MaxAge:     7,
            MaxBackups: 10,
            Compress:   true,
        }
    }

    return utils.NewStructuredLogger(config)
}
```

### YAML Configuration

```yaml
# config.yaml
logging:
  level: INFO
  format: json
  caller: true
  stack: false
  rotation:
    filename: /var/log/objectfs/app.log
    max_size_mb: 100
    max_age_days: 7
    max_backups: 10
    compress: true
```

```go
type LoggingConfig struct {
    Level    string `yaml:"level"`
    Format   string `yaml:"format"`
    Caller   bool   `yaml:"caller"`
    Stack    bool   `yaml:"stack"`
    Rotation struct {
        Filename    string `yaml:"filename"`
        MaxSize     int64  `yaml:"max_size_mb"`
        MaxAge      int    `yaml:"max_age_days"`
        MaxBackups  int    `yaml:"max_backups"`
        Compress    bool   `yaml:"compress"`
    } `yaml:"rotation"`
}

func loadConfig(filename string) (*utils.StructuredLogger, error) {
    data, err := os.ReadFile(filename)
    if err != nil {
        return nil, err
    }

    var cfg LoggingConfig
    if err := yaml.Unmarshal(data, &cfg); err != nil {
        return nil, err
    }

    level, _ := utils.ParseLogLevel(cfg.Level)

    format := utils.FormatText
    if cfg.Format == "json" {
        format = utils.FormatJSON
    }

    config := &utils.StructuredLoggerConfig{
        Level:         level,
        Format:        format,
        IncludeCaller: cfg.Caller,
        IncludeStack:  cfg.Stack,
    }

    if cfg.Rotation.Filename != "" {
        config.Rotation = &utils.RotationConfig{
            Filename:   cfg.Rotation.Filename,
            MaxSize:    cfg.Rotation.MaxSize,
            MaxAge:     cfg.Rotation.MaxAge,
            MaxBackups: cfg.Rotation.MaxBackups,
            Compress:   cfg.Rotation.Compress,
        }
    }

    return utils.NewStructuredLogger(config)
}
```

## Best Practices

### 1. Use Appropriate Log Levels

```go
// TRACE: Very detailed, temporary debugging
logger.Trace("Entering function", map[string]interface{}{
    "args": args,
})

// DEBUG: Development/troubleshooting info
logger.Debug("Cache lookup", map[string]interface{}{
    "key": key,
    "hit": true,
})

// INFO: General operational messages
logger.Info("Server started", map[string]interface{}{
    "port": 8080,
    "version": version,
})

// WARN: Something unexpected but not critical
logger.Warn("Slow operation detected", map[string]interface{}{
    "operation": "s3_read",
    "duration_ms": 5000,
})

// ERROR: Errors that need attention
logger.Error("Failed to write file", map[string]interface{}{
    "path": path,
    "error": err.Error(),
})

// FATAL: Critical errors, program exits
logger.Fatal("Cannot connect to database", map[string]interface{}{
    "error": err.Error(),
})
```

### 2. Use Structured Fields Instead of String Formatting

```go
// Bad: Information lost in formatted string
logger.Info(fmt.Sprintf("User %d logged in from %s", userID, ip))

// Good: Structured and searchable
logger.Info("User logged in", map[string]interface{}{
    "user_id": userID,
    "ip": ip,
})
```

### 3. Create Context Loggers for Related Operations

```go
// Create logger with context for entire request
requestLogger := logger.WithFields(map[string]interface{}{
    "request_id": requestID,
    "user_id": userID,
    "ip": req.RemoteAddr,
})

// All logs in request handling will include context
requestLogger.Info("Processing upload")
requestLogger.Debug("Validating file")
requestLogger.Info("Upload complete")
```

### 4. Handle Errors Consistently

```go
func processFile(path string, logger *utils.StructuredLogger) error {
    file, err := os.Open(path)
    if err != nil {
        logger.Error("Failed to open file", map[string]interface{}{
            "path": path,
            "error": err.Error(),
        })
        return fmt.Errorf("open file: %w", err)
    }
    defer file.Close()

    // Process file...

    return nil
}
```

### 5. Use Debug Mode for Complex Issues

```go
// Enable debug session for specific request
if strings.HasPrefix(requestID, "debug-") {
    dm := utils.GetDebugManager()
    session := dm.StartSession(requestID, []string{"storage", "cache"}, 1000)
    defer dm.StopSession(requestID)

    ctx = utils.WithContext(ctx, requestID)
}

// Normal request processing with optional tracing
processRequest(ctx, req)
```

### 6. Flush Logs on Shutdown

```go
func main() {
    logger, err := setupLogger()
    if err != nil {
        log.Fatal(err)
    }

    // Ensure logs are flushed on exit
    defer func() {
        logger.Sync()
        logger.Close()
    }()

    // Run application...
}
```

## Examples

### Example 1: HTTP Server with Request Logging

```go
func loggingMiddleware(logger *utils.StructuredLogger) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            start := time.Now()
            requestID := generateRequestID()

            // Create request logger
            reqLogger := logger.WithFields(map[string]interface{}{
                "request_id": requestID,
                "method": r.Method,
                "path": r.URL.Path,
                "remote_addr": r.RemoteAddr,
            })

            reqLogger.Info("Request started")

            // Process request
            next.ServeHTTP(w, r)

            // Log completion
            reqLogger.Info("Request completed", map[string]interface{}{
                "duration_ms": time.Since(start).Milliseconds(),
            })
        })
    }
}
```

### Example 2: Background Job with Rotation

```go
func runBackgroundJob(logger *utils.StructuredLogger) {
    jobLogger := logger.WithComponent("background_job")

    ticker := time.NewTicker(1 * time.Hour)
    defer ticker.Stop()

    for range ticker.C {
        start := time.Now()

        jobLogger.Info("Job started")

        if err := performWork(); err != nil {
            jobLogger.Error("Job failed", map[string]interface{}{
                "error": err.Error(),
                "duration": time.Since(start),
            })
            continue
        }

        jobLogger.Info("Job completed", map[string]interface{}{
            "duration": time.Since(start),
        })
    }
}
```

### Example 3: Debug Session for Performance Analysis

```go
func analyzePerformance() {
    // Start debug session
    dm := utils.GetDebugManager()
    sessionID := fmt.Sprintf("perf-analysis-%d", time.Now().Unix())
    session := dm.StartSession(sessionID, nil, 10000)

    ctx := utils.WithContext(context.Background(), sessionID)

    // Run operations with tracing
    for i := 0; i < 100; i++ {
        trace := utils.StartTrace(sessionID, "storage", "read", map[string]interface{}{
            "iteration": i,
        })

        performRead()

        trace.End("Read completed")
    }

    // Analyze results
    finalSession := dm.StopSession(sessionID)
    events := finalSession.GetEvents()

    // Calculate statistics
    var totalDuration time.Duration
    for _, event := range events {
        totalDuration += event.Duration
    }

    avgDuration := totalDuration / time.Duration(len(events))
    fmt.Printf("Average read duration: %v\n", avgDuration)

    // Find slowest operations
    sort.Slice(events, func(i, j int) bool {
        return events[i].Duration > events[j].Duration
    })

    fmt.Println("\nTop 10 slowest operations:")
    for i := 0; i < 10 && i < len(events); i++ {
        fmt.Printf("%d. %v - %s\n", i+1, events[i].Duration, events[i].Message)
    }
}
```

## Troubleshooting

### Log File Permission Issues

```go
// Ensure log directory exists and is writable
logDir := filepath.Dir(config.Rotation.Filename)
if err := os.MkdirAll(logDir, 0755); err != nil {
    return fmt.Errorf("create log directory: %w", err)
}
```

### Rotation Not Working

Check that:
1. `MaxSize` or `MaxAge` is set (both 0 means no rotation)
2. Log file path is writable
3. Sufficient disk space available

```go
// Test rotation manually
if rotator, ok := logger.(*utils.LogRotator); ok {
    if err := rotator.ForceRotate(); err != nil {
        fmt.Printf("Rotation test failed: %v\n", err)
    }
}
```

### High Memory Usage in Debug Mode

Limit event recording:

```go
// Set smaller max events
session := dm.StartSession(sessionID, components, 1000) // Instead of 10000

// Or filter specific components
components := []string{"storage"} // Only track storage
session := dm.StartSession(sessionID, components, 5000)
```

### Missing Log Messages

Check log levels:

```go
// Verify logger level
currentLevel := logger.GetLevel()
fmt.Printf("Current log level: %s\n", currentLevel)

// Check component-specific levels
logger.SetComponentLevel("mycomponent", utils.DEBUG)
```
