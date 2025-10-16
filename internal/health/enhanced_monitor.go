package health

import (
	"context"
	"fmt"
	"sync"
	"time"

	pkghealth "github.com/objectfs/objectfs/pkg/health"
)

// EnhancedMonitor extends Monitor with improved problem detection and remediation
type EnhancedMonitor struct {
	*Monitor
	tracker     *pkghealth.Tracker // Component health tracker
	remediation *RemediationEngine
	analyzer    *ProblemAnalyzer
	diagnoses   map[string]*ProblemDiagnosis
	diagMu      sync.RWMutex
}

// ProblemAnalyzer analyzes health patterns and detects problems
type ProblemAnalyzer struct {
	mu             sync.RWMutex
	patterns       map[string]*HealthPattern
	thresholds     *AnalysisThresholds
	detectedIssues []*DetectedIssue
}

// HealthPattern tracks health patterns for a component
type HealthPattern struct {
	ComponentName   string
	RecentResults   []*Result
	ErrorRate       float64
	LatencyTrend    string // "increasing", "decreasing", "stable"
	AvgLatency      time.Duration
	FailureStreak   int
	LastSuccess     time.Time
	LastFailure     time.Time
	PredictedStatus Status
}

// AnalysisThresholds defines thresholds for problem detection
type AnalysisThresholds struct {
	ErrorRateWarning      float64       // e.g., 0.1 = 10%
	ErrorRateCritical     float64       // e.g., 0.5 = 50%
	LatencyWarning        time.Duration // e.g., 500ms
	LatencyCritical       time.Duration // e.g., 2s
	FailureStreakWarning  int           // e.g., 3
	FailureStreakCritical int           // e.g., 5
	PatternWindow         int           // Number of recent results to analyze
}

// DetectedIssue represents an automatically detected health issue
type DetectedIssue struct {
	ID              string            `json:"id"`
	Component       string            `json:"component"`
	IssueType       string            `json:"issue_type"`
	Severity        Priority          `json:"severity"`
	Description     string            `json:"description"`
	DetectedAt      time.Time         `json:"detected_at"`
	Pattern         *HealthPattern    `json:"pattern,omitempty"`
	Diagnosis       *ProblemDiagnosis `json:"diagnosis,omitempty"`
	Resolved        bool              `json:"resolved"`
	ResolvedAt      *time.Time        `json:"resolved_at,omitempty"`
	AutoRemediating bool              `json:"auto_remediating"`
}

// ComponentHealthDetail provides detailed component health information
type ComponentHealthDetail struct {
	*pkghealth.ComponentHealth
	Pattern         *HealthPattern         `json:"pattern"`
	RecentDiagnoses []*ProblemDiagnosis    `json:"recent_diagnoses"`
	ActiveIssues    []*DetectedIssue       `json:"active_issues"`
	HealthScore     float64                `json:"health_score"` // 0-100
	Recommendations []*RemediationAction   `json:"recommendations"`
	Trends          map[string]interface{} `json:"trends"`
}

// NewEnhancedMonitor creates an enhanced monitor with problem detection and remediation
func NewEnhancedMonitor(config *MonitorConfig) (*EnhancedMonitor, error) {
	baseMonitor, err := NewMonitor(config)
	if err != nil {
		return nil, err
	}

	thresholds := &AnalysisThresholds{
		ErrorRateWarning:      0.1,
		ErrorRateCritical:     0.5,
		LatencyWarning:        500 * time.Millisecond,
		LatencyCritical:       2 * time.Second,
		FailureStreakWarning:  3,
		FailureStreakCritical: 5,
		PatternWindow:         20,
	}

	// Create component health tracker
	tracker := pkghealth.NewTracker(pkghealth.DefaultConfig())

	enhanced := &EnhancedMonitor{
		Monitor:     baseMonitor,
		tracker:     tracker,
		remediation: NewRemediationEngine(),
		analyzer: &ProblemAnalyzer{
			patterns:       make(map[string]*HealthPattern),
			thresholds:     thresholds,
			detectedIssues: make([]*DetectedIssue, 0),
		},
		diagnoses: make(map[string]*ProblemDiagnosis),
	}

	return enhanced, nil
}

// Start starts the enhanced monitor with automatic problem detection
func (em *EnhancedMonitor) Start(ctx context.Context) error {
	// Start base monitor
	if err := em.Monitor.Start(ctx); err != nil {
		return err
	}

	// Start problem detection loop
	go em.problemDetectionLoop(ctx)

	return nil
}

// GetComponentHealthDetail returns detailed health information for a component
func (em *EnhancedMonitor) GetComponentHealthDetail(componentName string) (*ComponentHealthDetail, error) {
	// Get base component health from tracker
	baseHealth, err := em.tracker.GetComponentHealth(componentName)
	if err != nil {
		return nil, err
	}

	detail := &ComponentHealthDetail{
		ComponentHealth: baseHealth,
		Trends:          make(map[string]interface{}),
	}

	// Add pattern analysis
	em.analyzer.mu.RLock()
	if pattern, exists := em.analyzer.patterns[componentName]; exists {
		detail.Pattern = pattern
		detail.HealthScore = em.calculateHealthScore(pattern, baseHealth)
		detail.Trends["error_rate"] = pattern.ErrorRate
		detail.Trends["latency_trend"] = pattern.LatencyTrend
		detail.Trends["failure_streak"] = pattern.FailureStreak
	}
	em.analyzer.mu.RUnlock()

	// Add recent diagnoses
	em.diagMu.RLock()
	if diagnosis, exists := em.diagnoses[componentName]; exists {
		detail.RecentDiagnoses = []*ProblemDiagnosis{diagnosis}
		detail.Recommendations = diagnosis.Remediations
	}
	em.diagMu.RUnlock()

	// Add active issues
	em.analyzer.mu.RLock()
	for _, issue := range em.analyzer.detectedIssues {
		if issue.Component == componentName && !issue.Resolved {
			detail.ActiveIssues = append(detail.ActiveIssues, issue)
		}
	}
	em.analyzer.mu.RUnlock()

	return detail, nil
}

// DiagnoseComponent diagnoses a component and returns remediation recommendations
func (em *EnhancedMonitor) DiagnoseComponent(ctx context.Context, componentName string) (*ProblemDiagnosis, error) {
	// Trigger health check for the component
	result, err := em.Monitor.checker.RunCheck(ctx, componentName)
	if err != nil {
		return nil, fmt.Errorf("failed to run health check: %w", err)
	}

	// Get component health from tracker
	health, err := em.tracker.GetComponentHealth(componentName)
	if err != nil {
		// If component not in tracker, create minimal health structure
		health = &pkghealth.ComponentHealth{
			Name:              componentName,
			State:             pkghealth.StateUnavailable,
			ConsecutiveErrors: 0,
		}
	}

	// Generate diagnosis
	diagnosis := em.remediation.DiagnoseProblem(result, health)

	// Store diagnosis
	em.diagMu.Lock()
	em.diagnoses[componentName] = diagnosis
	em.diagMu.Unlock()

	return diagnosis, nil
}

// AttemptAutoRemediation attempts to automatically fix a component
func (em *EnhancedMonitor) AttemptAutoRemediation(ctx context.Context, componentName string) error {
	// Get diagnosis
	em.diagMu.RLock()
	diagnosis, exists := em.diagnoses[componentName]
	em.diagMu.RUnlock()

	if !exists {
		// Generate fresh diagnosis
		var err error
		diagnosis, err = em.DiagnoseComponent(ctx, componentName)
		if err != nil {
			return fmt.Errorf("failed to diagnose component: %w", err)
		}
	}

	// Attempt auto-remediation
	return em.remediation.AutoRemediate(ctx, diagnosis)
}

// GetAllComponentDetails returns detailed health for all components
func (em *EnhancedMonitor) GetAllComponentDetails() map[string]*ComponentHealthDetail {
	components := em.tracker.GetAllComponents()
	details := make(map[string]*ComponentHealthDetail)

	for name := range components {
		if detail, err := em.GetComponentHealthDetail(name); err == nil {
			details[name] = detail
		}
	}

	return details
}

// GetDetectedIssues returns all detected issues
func (em *EnhancedMonitor) GetDetectedIssues(includeResolved bool) []*DetectedIssue {
	em.analyzer.mu.RLock()
	defer em.analyzer.mu.RUnlock()

	issues := make([]*DetectedIssue, 0)
	for _, issue := range em.analyzer.detectedIssues {
		if includeResolved || !issue.Resolved {
			issues = append(issues, issue)
		}
	}

	return issues
}

// GetRemediationHistory returns recent remediation attempts
func (em *EnhancedMonitor) GetRemediationHistory(limit int) []RemediationAttempt {
	return em.remediation.GetRemediationHistory(limit)
}

// problemDetectionLoop continuously analyzes health patterns
func (em *EnhancedMonitor) problemDetectionLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			em.analyzeHealthPatterns()
		}
	}
}

// analyzeHealthPatterns analyzes health patterns and detects issues
func (em *EnhancedMonitor) analyzeHealthPatterns() {
	// Get all current health check results
	status := em.Monitor.checker.GetStatus()
	checks, ok := status["checks"].(map[string]*Result)
	if !ok {
		return
	}

	for checkName, result := range checks {
		em.updateHealthPattern(checkName, result)
		em.detectProblems(checkName)
	}
}

// updateHealthPattern updates the health pattern for a component
func (em *EnhancedMonitor) updateHealthPattern(checkName string, result *Result) {
	em.analyzer.mu.Lock()
	defer em.analyzer.mu.Unlock()

	pattern, exists := em.analyzer.patterns[checkName]
	if !exists {
		pattern = &HealthPattern{
			ComponentName: checkName,
			RecentResults: make([]*Result, 0, em.analyzer.thresholds.PatternWindow),
		}
		em.analyzer.patterns[checkName] = pattern
	}

	// Add result to recent results
	pattern.RecentResults = append(pattern.RecentResults, result)
	if len(pattern.RecentResults) > em.analyzer.thresholds.PatternWindow {
		pattern.RecentResults = pattern.RecentResults[1:]
	}

	// Update pattern metrics
	em.calculatePatternMetrics(pattern)

	// Update timestamps
	if result.Status == StatusHealthy {
		pattern.LastSuccess = result.Timestamp
		pattern.FailureStreak = 0
	} else if result.Status == StatusUnhealthy {
		pattern.LastFailure = result.Timestamp
		pattern.FailureStreak++
	}
}

// calculatePatternMetrics calculates metrics for a health pattern
func (em *EnhancedMonitor) calculatePatternMetrics(pattern *HealthPattern) {
	if len(pattern.RecentResults) == 0 {
		return
	}

	// Calculate error rate
	errorCount := 0
	var totalLatency time.Duration
	for _, result := range pattern.RecentResults {
		if result.Status == StatusUnhealthy {
			errorCount++
		}
		totalLatency += result.Duration
	}
	pattern.ErrorRate = float64(errorCount) / float64(len(pattern.RecentResults))
	pattern.AvgLatency = totalLatency / time.Duration(len(pattern.RecentResults))

	// Determine latency trend (simplified - compare first half to second half)
	if len(pattern.RecentResults) >= 4 {
		midpoint := len(pattern.RecentResults) / 2
		var firstHalfLatency, secondHalfLatency time.Duration

		for i := 0; i < midpoint; i++ {
			firstHalfLatency += pattern.RecentResults[i].Duration
		}
		for i := midpoint; i < len(pattern.RecentResults); i++ {
			secondHalfLatency += pattern.RecentResults[i].Duration
		}

		firstHalfAvg := firstHalfLatency / time.Duration(midpoint)
		secondHalfAvg := secondHalfLatency / time.Duration(len(pattern.RecentResults)-midpoint)

		if secondHalfAvg > firstHalfAvg*11/10 { // >10% increase
			pattern.LatencyTrend = "increasing"
		} else if secondHalfAvg < firstHalfAvg*9/10 { // >10% decrease
			pattern.LatencyTrend = "decreasing"
		} else {
			pattern.LatencyTrend = "stable"
		}
	}

	// Predict status based on trends
	if pattern.ErrorRate >= em.analyzer.thresholds.ErrorRateCritical {
		pattern.PredictedStatus = StatusUnhealthy
	} else if pattern.ErrorRate >= em.analyzer.thresholds.ErrorRateWarning {
		pattern.PredictedStatus = StatusDegraded
	} else if pattern.LatencyTrend == "increasing" && pattern.AvgLatency > em.analyzer.thresholds.LatencyWarning {
		pattern.PredictedStatus = StatusDegraded
	} else {
		pattern.PredictedStatus = StatusHealthy
	}
}

// detectProblems detects problems based on health patterns
func (em *EnhancedMonitor) detectProblems(checkName string) {
	em.analyzer.mu.Lock()
	defer em.analyzer.mu.Unlock()

	pattern, exists := em.analyzer.patterns[checkName]
	if !exists {
		return
	}

	// Check for high error rate
	if pattern.ErrorRate >= em.analyzer.thresholds.ErrorRateCritical {
		em.createDetectedIssue(checkName, "high_error_rate", PriorityCritical,
			fmt.Sprintf("Error rate %.1f%% exceeds critical threshold", pattern.ErrorRate*100),
			pattern)
	} else if pattern.ErrorRate >= em.analyzer.thresholds.ErrorRateWarning {
		em.createDetectedIssue(checkName, "elevated_error_rate", PriorityHigh,
			fmt.Sprintf("Error rate %.1f%% exceeds warning threshold", pattern.ErrorRate*100),
			pattern)
	}

	// Check for failure streak
	if pattern.FailureStreak >= em.analyzer.thresholds.FailureStreakCritical {
		em.createDetectedIssue(checkName, "failure_streak", PriorityCritical,
			fmt.Sprintf("%d consecutive failures detected", pattern.FailureStreak),
			pattern)
	} else if pattern.FailureStreak >= em.analyzer.thresholds.FailureStreakWarning {
		em.createDetectedIssue(checkName, "failure_streak", PriorityHigh,
			fmt.Sprintf("%d consecutive failures detected", pattern.FailureStreak),
			pattern)
	}

	// Check for increasing latency
	if pattern.LatencyTrend == "increasing" && pattern.AvgLatency > em.analyzer.thresholds.LatencyCritical {
		em.createDetectedIssue(checkName, "latency_degradation", PriorityHigh,
			fmt.Sprintf("Latency increasing, avg %v exceeds critical threshold", pattern.AvgLatency),
			pattern)
	} else if pattern.LatencyTrend == "increasing" && pattern.AvgLatency > em.analyzer.thresholds.LatencyWarning {
		em.createDetectedIssue(checkName, "latency_degradation", PriorityMedium,
			fmt.Sprintf("Latency increasing, avg %v exceeds warning threshold", pattern.AvgLatency),
			pattern)
	}
}

// createDetectedIssue creates a detected issue if it doesn't already exist
func (em *EnhancedMonitor) createDetectedIssue(component, issueType string, severity Priority, description string, pattern *HealthPattern) {
	// Check if issue already exists and is not resolved
	issueID := fmt.Sprintf("%s-%s", component, issueType)
	for _, existingIssue := range em.analyzer.detectedIssues {
		if existingIssue.ID == issueID && !existingIssue.Resolved {
			// Update existing issue
			existingIssue.Description = description
			existingIssue.Severity = severity
			existingIssue.Pattern = pattern
			return
		}
	}

	// Create new issue
	issue := &DetectedIssue{
		ID:          issueID,
		Component:   component,
		IssueType:   issueType,
		Severity:    severity,
		Description: description,
		DetectedAt:  time.Now(),
		Pattern:     pattern,
		Resolved:    false,
	}

	em.analyzer.detectedIssues = append(em.analyzer.detectedIssues, issue)

	// If auto-recovery enabled, attempt remediation
	if em.config.AutoRecovery && severity >= PriorityHigh {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			issue.AutoRemediating = true
			if err := em.AttemptAutoRemediation(ctx, component); err == nil {
				// Mark issue as resolved
				em.analyzer.mu.Lock()
				issue.Resolved = true
				now := time.Now()
				issue.ResolvedAt = &now
				issue.AutoRemediating = false
				em.analyzer.mu.Unlock()
			} else {
				issue.AutoRemediating = false
			}
		}()
	}
}

// calculateHealthScore calculates a 0-100 health score for a component
func (em *EnhancedMonitor) calculateHealthScore(pattern *HealthPattern, health *pkghealth.ComponentHealth) float64 {
	score := 100.0

	// Deduct for error rate
	score -= pattern.ErrorRate * 50 // Max -50 for 100% error rate

	// Deduct for consecutive errors
	if health.ConsecutiveErrors > 0 {
		score -= float64(health.ConsecutiveErrors) * 5 // -5 per consecutive error
	}

	// Deduct for high latency
	if pattern.AvgLatency > em.analyzer.thresholds.LatencyCritical {
		score -= 20
	} else if pattern.AvgLatency > em.analyzer.thresholds.LatencyWarning {
		score -= 10
	}

	// Deduct for increasing latency trend
	if pattern.LatencyTrend == "increasing" {
		score -= 10
	}

	// Ensure score is in valid range
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	return score
}

// ResolveIssue manually marks an issue as resolved
func (em *EnhancedMonitor) ResolveIssue(issueID string) error {
	em.analyzer.mu.Lock()
	defer em.analyzer.mu.Unlock()

	for _, issue := range em.analyzer.detectedIssues {
		if issue.ID == issueID {
			issue.Resolved = true
			now := time.Now()
			issue.ResolvedAt = &now
			return nil
		}
	}

	return fmt.Errorf("issue %s not found", issueID)
}
