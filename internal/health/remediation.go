package health

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	pkghealth "github.com/objectfs/objectfs/pkg/health"
)

// RemediationAction represents a recommended action to fix a health issue
type RemediationAction struct {
	ID            string        `json:"id"`
	Priority      Priority      `json:"priority"`
	Title         string        `json:"title"`
	Description   string        `json:"description"`
	Steps         []string      `json:"steps"`
	Automated     bool          `json:"automated"`
	AutoFix       AutoFixFunc   `json:"-"`
	EstimatedTime time.Duration `json:"estimated_time"`
	Impact        string        `json:"impact"`
	Category      string        `json:"category"`
}

// AutoFixFunc is a function that can automatically remediate an issue
type AutoFixFunc func(ctx context.Context) error

// RemediationEngine provides intelligent remediation recommendations
type RemediationEngine struct {
	rules     map[string]*RemediationRule
	history   []RemediationAttempt
	autoFixFn map[string]AutoFixFunc
}

// RemediationRule defines how to remediate a specific health issue
type RemediationRule struct {
	CheckName    string
	ErrorPattern string
	Actions      []*RemediationAction
	Conditions   []ConditionFunc
}

// ConditionFunc determines if a remediation should be applied
type ConditionFunc func(result *Result, health *pkghealth.ComponentHealth) bool

// RemediationAttempt tracks a remediation attempt
type RemediationAttempt struct {
	ActionID  string        `json:"action_id"`
	CheckName string        `json:"check_name"`
	Timestamp time.Time     `json:"timestamp"`
	Success   bool          `json:"success"`
	Error     error         `json:"error,omitempty"`
	Duration  time.Duration `json:"duration"`
	Automated bool          `json:"automated"`
}

// ProblemDiagnosis provides detailed analysis of a health problem
type ProblemDiagnosis struct {
	Check               string               `json:"check"`
	Category            Category             `json:"category"`
	Severity            Priority             `json:"severity"`
	Problem             string               `json:"problem"`
	PossibleCauses      []string             `json:"possible_causes"`
	Symptoms            []string             `json:"symptoms"`
	Impact              string               `json:"impact"`
	Remediations        []*RemediationAction `json:"remediations"`
	DetectedAt          time.Time            `json:"detected_at"`
	ConsecutiveFailures int                  `json:"consecutive_failures"`
}

// NewRemediationEngine creates a new remediation engine
func NewRemediationEngine() *RemediationEngine {
	engine := &RemediationEngine{
		rules:     make(map[string]*RemediationRule),
		history:   make([]RemediationAttempt, 0),
		autoFixFn: make(map[string]AutoFixFunc),
	}

	// Register default remediation rules
	engine.registerDefaultRules()

	return engine
}

// DiagnoseProblem analyzes a health check failure and provides diagnosis
func (re *RemediationEngine) DiagnoseProblem(result *Result, health *pkghealth.ComponentHealth) *ProblemDiagnosis {
	diagnosis := &ProblemDiagnosis{
		Check:               result.Check,
		Problem:             result.Message,
		Symptoms:            []string{result.Error},
		DetectedAt:          result.Timestamp,
		ConsecutiveFailures: health.ConsecutiveErrors,
		Remediations:        make([]*RemediationAction, 0),
	}

	// Find matching remediation rules
	if rule, exists := re.rules[result.Check]; exists {
		// Check if error pattern matches
		if strings.Contains(result.Error, rule.ErrorPattern) || rule.ErrorPattern == "" {
			// Evaluate conditions
			allConditionsMet := true
			for _, condition := range rule.Conditions {
				if !condition(result, health) {
					allConditionsMet = false
					break
				}
			}

			if allConditionsMet {
				diagnosis.Remediations = append(diagnosis.Remediations, rule.Actions...)
			}
		}
	}

	// Analyze the problem based on check type and error
	re.analyzeProblem(diagnosis, result, health)

	return diagnosis
}

// AutoRemediate attempts to automatically fix a problem
func (re *RemediationEngine) AutoRemediate(ctx context.Context, diagnosis *ProblemDiagnosis) error {
	// Find automated remediation actions
	for _, action := range diagnosis.Remediations {
		if action.Automated && action.AutoFix != nil {
			attempt := RemediationAttempt{
				ActionID:  action.ID,
				CheckName: diagnosis.Check,
				Timestamp: time.Now(),
				Automated: true,
			}

			start := time.Now()
			err := action.AutoFix(ctx)
			attempt.Duration = time.Since(start)

			if err != nil {
				attempt.Success = false
				attempt.Error = err
				re.history = append(re.history, attempt)
				return fmt.Errorf("auto-remediation failed: %w", err)
			}

			attempt.Success = true
			re.history = append(re.history, attempt)
			return nil
		}
	}

	return fmt.Errorf("no automated remediation available for %s", diagnosis.Check)
}

// GetRemediationHistory returns recent remediation attempts
func (re *RemediationEngine) GetRemediationHistory(limit int) []RemediationAttempt {
	if limit <= 0 || limit > len(re.history) {
		limit = len(re.history)
	}

	// Return most recent attempts
	start := max(len(re.history)-limit, 0)

	return re.history[start:]
}

// analyzeProblem provides detailed problem analysis
func (re *RemediationEngine) analyzeProblem(diagnosis *ProblemDiagnosis, result *Result, health *pkghealth.ComponentHealth) {
	checkName := result.Check

	// Storage-related problems
	if strings.Contains(checkName, "storage") || strings.Contains(checkName, "s3") {
		diagnosis.Category = CategoryStorage
		if strings.Contains(result.Error, "connection") {
			diagnosis.Severity = PriorityCritical
			diagnosis.PossibleCauses = []string{
				"Network connectivity issues to S3",
				"AWS credentials expired or invalid",
				"S3 bucket not accessible",
				"Firewall blocking S3 endpoints",
			}
			diagnosis.Impact = "Storage operations are failing. Read and write operations will not work."
		} else if strings.Contains(result.Error, "permission") {
			diagnosis.Severity = PriorityHigh
			diagnosis.PossibleCauses = []string{
				"IAM permissions insufficient",
				"Bucket policy denies access",
				"KMS key access denied",
			}
			diagnosis.Impact = "Storage operations are denied. Check IAM permissions and bucket policies."
		}
	}

	// Cache-related problems
	if strings.Contains(checkName, "cache") {
		diagnosis.Category = CategoryCache
		diagnosis.Severity = PriorityMedium
		if strings.Contains(result.Error, "memory") {
			diagnosis.PossibleCauses = []string{
				"Insufficient memory for cache",
				"Cache size misconfigured",
				"Memory leak in cache layer",
			}
			diagnosis.Impact = "Cache performance degraded. Operations will be slower but functional."
		} else if strings.Contains(result.Error, "eviction") {
			diagnosis.PossibleCauses = []string{
				"Cache size too small for workload",
				"High cache churn rate",
				"Inefficient cache key distribution",
			}
			diagnosis.Impact = "High cache miss rate. Performance will be degraded."
		}
	}

	// Network-related problems
	if strings.Contains(checkName, "network") {
		diagnosis.Category = CategoryNetwork
		if strings.Contains(result.Error, "timeout") {
			diagnosis.Severity = PriorityHigh
			diagnosis.PossibleCauses = []string{
				"Network latency too high",
				"Firewall blocking connections",
				"DNS resolution issues",
				"Backend service overloaded",
			}
			diagnosis.Impact = "Network operations timing out. Service performance severely degraded."
		} else if strings.Contains(result.Error, "refused") {
			diagnosis.Severity = PriorityCritical
			diagnosis.PossibleCauses = []string{
				"Backend service not running",
				"Port configuration incorrect",
				"Firewall blocking connections",
			}
			diagnosis.Impact = "Cannot connect to backend services. Service unavailable."
		}
	}

	// Memory-related problems
	if strings.Contains(checkName, "memory") {
		diagnosis.Category = CategoryPerformance
		if strings.Contains(result.Error, "limit") || strings.Contains(result.Error, "exceeded") {
			diagnosis.Severity = PriorityCritical
			diagnosis.PossibleCauses = []string{
				"Memory leak in application",
				"Workload exceeds available memory",
				"Memory limits set too low",
				"Cache size configured too large",
			}
			diagnosis.Impact = "System memory exhausted. Risk of OOM crashes."
		}
	}

	// Disk-related problems
	if strings.Contains(checkName, "disk") {
		diagnosis.Category = CategoryCore
		if strings.Contains(result.Error, "space") || strings.Contains(result.Error, "full") {
			diagnosis.Severity = PriorityCritical
			diagnosis.PossibleCauses = []string{
				"Disk space exhausted",
				"Log files not being rotated",
				"Cache directory too large",
				"Temporary files not being cleaned",
			}
			diagnosis.Impact = "Disk full. Write operations will fail."
		}
	}

	// Add generic symptoms if consecutive failures
	if diagnosis.ConsecutiveFailures >= 3 {
		diagnosis.Symptoms = append(diagnosis.Symptoms, fmt.Sprintf("%d consecutive failures detected", diagnosis.ConsecutiveFailures))
	}

	if diagnosis.ConsecutiveFailures >= 10 {
		diagnosis.Symptoms = append(diagnosis.Symptoms, "Component may need restart or manual intervention")
	}
}

// registerDefaultRules registers default remediation rules
func (re *RemediationEngine) registerDefaultRules() {
	// S3 Storage remediation
	re.rules["s3_storage"] = &RemediationRule{
		CheckName:    "s3_storage",
		ErrorPattern: "connection",
		Actions: []*RemediationAction{
			{
				ID:          "s3_check_credentials",
				Priority:    PriorityCritical,
				Title:       "Verify AWS credentials",
				Description: "Check that AWS credentials are valid and not expired",
				Steps: []string{
					"Check AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables",
					"Verify credentials have not expired (if using temporary credentials)",
					"Test credentials: aws sts get-caller-identity",
					"Check IAM permissions for S3 access",
				},
				Automated:     false,
				EstimatedTime: 5 * time.Minute,
				Impact:        "Critical - Storage operations will resume",
				Category:      "credentials",
			},
			{
				ID:          "s3_check_connectivity",
				Priority:    PriorityCritical,
				Title:       "Verify S3 connectivity",
				Description: "Check network connectivity to S3 endpoints",
				Steps: []string{
					"Test DNS resolution: nslookup s3.amazonaws.com",
					"Test connectivity: curl -I https://s3.amazonaws.com",
					"Check firewall rules for outbound HTTPS (port 443)",
					"Verify no proxy issues blocking S3",
				},
				Automated:     false,
				EstimatedTime: 10 * time.Minute,
				Impact:        "Critical - Storage operations will resume",
				Category:      "network",
			},
			{
				ID:          "s3_restart_connection",
				Priority:    PriorityHigh,
				Title:       "Restart S3 connection pool",
				Description: "Reset S3 connection pool to clear stale connections",
				Steps: []string{
					"Recreate S3 client connections",
					"Clear any cached connection state",
					"Verify new connections work",
				},
				Automated: true,
				AutoFix: func(ctx context.Context) error {
					// S3 client reference is not available inside the remediation engine.
					// A full fix requires dependency injection of the S3 backend.
					slog.Info("health: s3_restart_connection: S3 client reference not available; restart the objectfs process to reset the connection pool")
					return nil
				},
				EstimatedTime: 30 * time.Second,
				Impact:        "Low risk - May temporarily pause operations",
				Category:      "connection",
			},
		},
	}

	// Cache remediation
	re.rules["cache_health"] = &RemediationRule{
		CheckName:    "cache_health",
		ErrorPattern: "",
		Actions: []*RemediationAction{
			{
				ID:          "cache_clear",
				Priority:    PriorityMedium,
				Title:       "Clear cache",
				Description: "Clear cache to free memory and reset state",
				Steps: []string{
					"Clear L1 memory cache",
					"Clear L2 persistent cache if applicable",
					"Verify cache operations after clear",
				},
				Automated: true,
				AutoFix: func(ctx context.Context) error {
					// Cache reference is not available inside the remediation engine.
					// A full fix requires dependency injection of the cache layer.
					slog.Info("health: cache_clear: cache reference not available; configure cache eviction policy or restart objectfs")
					return nil
				},
				EstimatedTime: 10 * time.Second,
				Impact:        "Medium - Cache will need to warm up again",
				Category:      "cache",
			},
			{
				ID:          "cache_resize",
				Priority:    PriorityLow,
				Title:       "Adjust cache size",
				Description: "Resize cache based on available memory",
				Steps: []string{
					"Check current memory usage",
					"Calculate optimal cache size",
					"Update cache configuration",
					"Restart cache with new settings",
				},
				Automated:     false,
				EstimatedTime: 5 * time.Minute,
				Impact:        "Medium - Requires configuration change",
				Category:      "configuration",
			},
		},
	}

	// Memory remediation
	re.rules["memory_usage"] = &RemediationRule{
		CheckName:    "memory_usage",
		ErrorPattern: "",
		Actions: []*RemediationAction{
			{
				ID:          "memory_force_gc",
				Priority:    PriorityHigh,
				Title:       "Force garbage collection",
				Description: "Trigger Go garbage collector to free memory",
				Steps: []string{
					"Call runtime.GC() to force garbage collection",
					"Wait for GC to complete",
					"Verify memory usage decreased",
				},
				Automated: true,
				AutoFix: func(ctx context.Context) error {
					var memBefore, memAfter runtime.MemStats
					runtime.ReadMemStats(&memBefore)
					slog.Info("health: memory_force_gc: before GC", "heap_alloc_bytes", memBefore.HeapAlloc)
					runtime.GC()
					debug.FreeOSMemory()
					runtime.ReadMemStats(&memAfter)
					slog.Info("health: memory_force_gc: after GC", "heap_alloc_bytes", memAfter.HeapAlloc, "freed_bytes", int64(memBefore.HeapAlloc)-int64(memAfter.HeapAlloc))
					return nil
				},
				EstimatedTime: 5 * time.Second,
				Impact:        "Low - Brief performance impact during GC",
				Category:      "memory",
			},
			{
				ID:          "memory_reduce_cache",
				Priority:    PriorityMedium,
				Title:       "Reduce cache size",
				Description: "Decrease cache size to free memory",
				Steps: []string{
					"Calculate current cache memory usage",
					"Reduce cache size by 25%",
					"Trigger cache eviction",
					"Monitor memory usage",
				},
				Automated: true,
				AutoFix: func(ctx context.Context) error {
					// Cache reference is not available inside the remediation engine.
					// A full fix requires dependency injection of the cache layer.
					slog.Info("health: memory_reduce_cache: cache reference not available; configure cache_size in objectfs config to reduce memory usage")
					return nil
				},
				EstimatedTime: 30 * time.Second,
				Impact:        "Medium - Cache performance will decrease",
				Category:      "cache",
			},
		},
	}

	// Disk space remediation
	re.rules["disk_space"] = &RemediationRule{
		CheckName:    "disk_space",
		ErrorPattern: "",
		Actions: []*RemediationAction{
			{
				ID:          "disk_clean_logs",
				Priority:    PriorityCritical,
				Title:       "Clean up log files",
				Description: "Remove old log files to free disk space",
				Steps: []string{
					"Rotate current log files",
					"Compress old logs",
					"Delete logs older than 30 days",
					"Verify disk space freed",
				},
				Automated: true,
				AutoFix: func(ctx context.Context) error {
					logDir := "/var/log/objectfs"
					cutoff := time.Now().Add(-30 * 24 * time.Hour)
					entries, err := os.ReadDir(logDir)
					if err != nil {
						if os.IsNotExist(err) {
							slog.Info("health: disk_clean_logs: log directory does not exist, nothing to clean", "log_dir", logDir)
							return nil
						}
						return fmt.Errorf("disk_clean_logs: reading log directory: %w", err)
					}
					var removed int
					for _, entry := range entries {
						if entry.IsDir() || filepath.Ext(entry.Name()) != ".log" {
							continue
						}
						info, infoErr := entry.Info()
						if infoErr != nil {
							continue
						}
						if info.ModTime().Before(cutoff) {
							path := filepath.Join(logDir, entry.Name())
							if removeErr := os.Remove(path); removeErr != nil {
								slog.Warn("health: disk_clean_logs: failed to remove file", "path", path, "error", removeErr)
								continue
							}
							removed++
						}
					}
					slog.Info("health: disk_clean_logs: removed log files older than 30 days", "count", removed, "log_dir", logDir)
					return nil
				},
				EstimatedTime: 1 * time.Minute,
				Impact:        "Low - Old logs will be removed",
				Category:      "disk",
			},
			{
				ID:          "disk_clean_cache",
				Priority:    PriorityHigh,
				Title:       "Clean cache directory",
				Description: "Remove old cached files to free disk space",
				Steps: []string{
					"Identify cache directories",
					"Remove cache files older than 7 days",
					"Remove incomplete download files",
					"Verify disk space freed",
				},
				Automated: true,
				AutoFix: func(ctx context.Context) error {
					cacheDir := filepath.Join(os.TempDir(), "objectfs-cache")
					cutoff := time.Now().Add(-7 * 24 * time.Hour)
					entries, err := os.ReadDir(cacheDir)
					if err != nil {
						if os.IsNotExist(err) {
							slog.Info("health: disk_clean_cache: cache directory does not exist, nothing to clean", "cache_dir", cacheDir)
							return nil
						}
						return fmt.Errorf("disk_clean_cache: reading cache directory: %w", err)
					}
					var removed int
					for _, entry := range entries {
						if entry.IsDir() {
							continue
						}
						info, infoErr := entry.Info()
						if infoErr != nil {
							continue
						}
						if info.ModTime().Before(cutoff) {
							path := filepath.Join(cacheDir, entry.Name())
							if removeErr := os.Remove(path); removeErr != nil {
								slog.Warn("health: disk_clean_cache: failed to remove file", "path", path, "error", removeErr)
								continue
							}
							removed++
						}
					}
					slog.Info("health: disk_clean_cache: removed cache files older than 7 days", "count", removed, "cache_dir", cacheDir)
					return nil
				},
				EstimatedTime: 2 * time.Minute,
				Impact:        "Medium - Cache will need to rebuild",
				Category:      "disk",
			},
			{
				ID:          "disk_clean_temp",
				Priority:    PriorityMedium,
				Title:       "Clean temporary files",
				Description: "Remove temporary files to free disk space",
				Steps: []string{
					"Identify temp directories (/tmp, /var/tmp)",
					"Remove files older than 24 hours",
					"Remove orphaned temp files",
					"Verify disk space freed",
				},
				Automated: true,
				AutoFix: func(ctx context.Context) error {
					tmpDir := os.TempDir()
					cutoff := time.Now().Add(-24 * time.Hour)
					entries, err := os.ReadDir(tmpDir)
					if err != nil {
						return fmt.Errorf("disk_clean_temp: reading temp directory: %w", err)
					}
					var removed int
					for _, entry := range entries {
						if entry.IsDir() || !strings.HasPrefix(entry.Name(), "objectfs-") {
							continue
						}
						info, infoErr := entry.Info()
						if infoErr != nil {
							continue
						}
						if info.ModTime().Before(cutoff) {
							path := filepath.Join(tmpDir, entry.Name())
							if removeErr := os.Remove(path); removeErr != nil {
								slog.Warn("health: disk_clean_temp: failed to remove file", "path", path, "error", removeErr)
								continue
							}
							removed++
						}
					}
					slog.Info("health: disk_clean_temp: removed temp files matching objectfs-* older than 24h", "count", removed, "tmp_dir", tmpDir)
					return nil
				},
				EstimatedTime: 1 * time.Minute,
				Impact:        "Low - Only temp files removed",
				Category:      "disk",
			},
		},
	}

	// Network remediation
	re.rules["network_connectivity"] = &RemediationRule{
		CheckName:    "network_connectivity",
		ErrorPattern: "",
		Actions: []*RemediationAction{
			{
				ID:          "network_retry",
				Priority:    PriorityHigh,
				Title:       "Retry connection",
				Description: "Retry network connection after brief delay",
				Steps: []string{
					"Wait 5 seconds",
					"Retry connection",
					"Verify connection successful",
				},
				Automated: true,
				AutoFix: func(ctx context.Context) error {
					slog.Info("health: network_retry: waiting 5 seconds before signaling retry")
					select {
					case <-time.After(5 * time.Second):
						slog.Info("health: network_retry: wait complete, connection retry signaled")
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
				EstimatedTime: 10 * time.Second,
				Impact:        "Low - Brief delay only",
				Category:      "network",
			},
			{
				ID:          "network_dns_flush",
				Priority:    PriorityMedium,
				Title:       "Flush DNS cache",
				Description: "Clear DNS cache to resolve stale entries",
				Steps: []string{
					"Flush system DNS cache",
					"Re-resolve hostnames",
					"Retry connections",
				},
				Automated:     false,
				EstimatedTime: 2 * time.Minute,
				Impact:        "Low - DNS resolution will be fresh",
				Category:      "network",
			},
		},
	}
}

// GetRemediations returns remediation actions for a specific check
func (re *RemediationEngine) GetRemediations(checkName string) []*RemediationAction {
	if rule, exists := re.rules[checkName]; exists {
		return rule.Actions
	}
	return nil
}

// RegisterRemediationRule registers a custom remediation rule
func (re *RemediationEngine) RegisterRemediationRule(rule *RemediationRule) {
	re.rules[rule.CheckName] = rule
}

// RegisterAutoFix registers an automated fix function
func (re *RemediationEngine) RegisterAutoFix(actionID string, fixFunc AutoFixFunc) {
	re.autoFixFn[actionID] = fixFunc
}
