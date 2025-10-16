package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pkghealth "github.com/objectfs/objectfs/pkg/health"
)

func TestRemediationEngine_DiagnoseProblem(t *testing.T) {
	engine := NewRemediationEngine()

	tests := []struct {
		name           string
		result         *Result
		health         *pkghealth.ComponentHealth
		expectCategory Category
		expectActions  bool
	}{
		{
			name: "S3 connection error",
			result: &Result{
				Check:     "s3_storage",
				Status:    StatusUnhealthy,
				Message:   "Connection failed",
				Error:     "connection timeout to s3.amazonaws.com",
				Timestamp: time.Now(),
			},
			health: &pkghealth.ComponentHealth{
				Name:              "s3_storage",
				ConsecutiveErrors: 3,
			},
			expectCategory: CategoryStorage,
			expectActions:  true,
		},
		{
			name: "Cache memory issue",
			result: &Result{
				Check:     "cache_health",
				Status:    StatusUnhealthy,
				Message:   "Cache failure",
				Error:     "insufficient memory for cache allocation",
				Timestamp: time.Now(),
			},
			health: &pkghealth.ComponentHealth{
				Name:              "cache_health",
				ConsecutiveErrors: 2,
			},
			expectCategory: CategoryCache,
			expectActions:  true,
		},
		{
			name: "Network timeout",
			result: &Result{
				Check:     "network_connectivity",
				Status:    StatusUnhealthy,
				Message:   "Network check failed",
				Error:     "connection timeout after 10s",
				Timestamp: time.Now(),
			},
			health: &pkghealth.ComponentHealth{
				Name:              "network_connectivity",
				ConsecutiveErrors: 1,
			},
			expectCategory: CategoryNetwork,
			expectActions:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diagnosis := engine.DiagnoseProblem(tt.result, tt.health)

			if diagnosis == nil {
				t.Fatal("Expected diagnosis, got nil")
			}

			if diagnosis.Check != tt.result.Check {
				t.Errorf("Expected check %s, got %s", tt.result.Check, diagnosis.Check)
			}

			if diagnosis.Category != tt.expectCategory {
				t.Errorf("Expected category %s, got %s", tt.expectCategory, diagnosis.Category)
			}

			if tt.expectActions && len(diagnosis.Remediations) == 0 {
				t.Error("Expected remediation actions, got none")
			}

			if len(diagnosis.PossibleCauses) == 0 {
				t.Error("Expected possible causes, got none")
			}
		})
	}
}

func TestRemediationEngine_AutoRemediate(t *testing.T) {
	engine := NewRemediationEngine()

	// Register a test auto-fix function
	fixCalled := false
	testAutoFix := func(ctx context.Context) error {
		fixCalled = true
		return nil
	}

	action := &RemediationAction{
		ID:          "test_fix",
		Priority:    PriorityHigh,
		Title:       "Test fix",
		Description: "Test automated fix",
		Automated:   true,
		AutoFix:     testAutoFix,
	}

	diagnosis := &ProblemDiagnosis{
		Check:        "test_check",
		Remediations: []*RemediationAction{action},
	}

	ctx := context.Background()
	err := engine.AutoRemediate(ctx, diagnosis)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if !fixCalled {
		t.Error("Expected auto-fix function to be called")
	}

	// Check history
	history := engine.GetRemediationHistory(10)
	if len(history) != 1 {
		t.Errorf("Expected 1 history entry, got %d", len(history))
	}

	if !history[0].Success {
		t.Error("Expected remediation to be successful")
	}
}

func TestRemediationEngine_AutoRemediateFailure(t *testing.T) {
	engine := NewRemediationEngine()

	// Register a failing auto-fix function
	testAutoFix := func(ctx context.Context) error {
		return errors.New("fix failed")
	}

	action := &RemediationAction{
		ID:          "test_fix",
		Priority:    PriorityHigh,
		Title:       "Test fix",
		Description: "Test automated fix that fails",
		Automated:   true,
		AutoFix:     testAutoFix,
	}

	diagnosis := &ProblemDiagnosis{
		Check:        "test_check",
		Remediations: []*RemediationAction{action},
	}

	ctx := context.Background()
	err := engine.AutoRemediate(ctx, diagnosis)

	if err == nil {
		t.Error("Expected error, got nil")
	}

	// Check history
	history := engine.GetRemediationHistory(10)
	if len(history) != 1 {
		t.Errorf("Expected 1 history entry, got %d", len(history))
	}

	if history[0].Success {
		t.Error("Expected remediation to fail")
	}

	if history[0].Error == nil {
		t.Error("Expected error in history")
	}
}

func TestRemediationEngine_NoAutomatedRemediation(t *testing.T) {
	engine := NewRemediationEngine()

	action := &RemediationAction{
		ID:          "manual_fix",
		Priority:    PriorityHigh,
		Title:       "Manual fix",
		Description: "Manual remediation only",
		Automated:   false, // Not automated
	}

	diagnosis := &ProblemDiagnosis{
		Check:        "test_check",
		Remediations: []*RemediationAction{action},
	}

	ctx := context.Background()
	err := engine.AutoRemediate(ctx, diagnosis)

	if err == nil {
		t.Error("Expected error for no automated remediation, got nil")
	}

	if !strings.Contains(err.Error(), "no automated remediation") {
		t.Errorf("Expected 'no automated remediation' error, got %v", err)
	}
}

func TestRemediationEngine_GetRemediations(t *testing.T) {
	engine := NewRemediationEngine()

	// Check default rules are registered
	actions := engine.GetRemediations("s3_storage")
	if actions == nil {
		t.Fatal("Expected remediation actions for s3_storage, got nil")
	}

	if len(actions) == 0 {
		t.Error("Expected multiple remediation actions, got none")
	}

	// Verify action structure
	for _, action := range actions {
		if action.ID == "" {
			t.Error("Action ID should not be empty")
		}
		if action.Title == "" {
			t.Error("Action title should not be empty")
		}
		if len(action.Steps) == 0 {
			t.Error("Action should have steps")
		}
	}
}

func TestRemediationEngine_RegisterCustomRule(t *testing.T) {
	engine := NewRemediationEngine()

	customRule := &RemediationRule{
		CheckName:    "custom_check",
		ErrorPattern: "custom error",
		Actions: []*RemediationAction{
			{
				ID:          "custom_action",
				Priority:    PriorityMedium,
				Title:       "Custom action",
				Description: "Custom remediation",
				Steps:       []string{"Step 1", "Step 2"},
			},
		},
	}

	engine.RegisterRemediationRule(customRule)

	actions := engine.GetRemediations("custom_check")
	if actions == nil {
		t.Fatal("Expected custom remediation actions, got nil")
	}

	if len(actions) != 1 {
		t.Errorf("Expected 1 action, got %d", len(actions))
	}

	if actions[0].ID != "custom_action" {
		t.Errorf("Expected action ID 'custom_action', got %s", actions[0].ID)
	}
}

func TestRemediationEngine_DiagnosisAnalysis(t *testing.T) {
	engine := NewRemediationEngine()

	tests := []struct {
		name                 string
		checkName            string
		errorMessage         string
		expectPossibleCauses bool
	}{
		{
			name:                 "Storage connection error",
			checkName:            "s3_storage",
			errorMessage:         "connection timeout",
			expectPossibleCauses: true,
		},
		{
			name:                 "Storage permission error",
			checkName:            "s3_storage",
			errorMessage:         "permission denied",
			expectPossibleCauses: true,
		},
		{
			name:                 "Cache memory error",
			checkName:            "cache_health",
			errorMessage:         "insufficient memory",
			expectPossibleCauses: true,
		},
		{
			name:                 "Disk space error",
			checkName:            "disk_space",
			errorMessage:         "disk full",
			expectPossibleCauses: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &Result{
				Check:     tt.checkName,
				Status:    StatusUnhealthy,
				Message:   "Check failed",
				Error:     tt.errorMessage,
				Timestamp: time.Now(),
			}

			health := &pkghealth.ComponentHealth{
				Name:              tt.checkName,
				ConsecutiveErrors: 3,
			}

			diagnosis := engine.DiagnoseProblem(result, health)

			if tt.expectPossibleCauses && len(diagnosis.PossibleCauses) == 0 {
				t.Error("Expected possible causes, got none")
			}

			if diagnosis.Impact == "" {
				t.Error("Expected impact analysis, got empty string")
			}
		})
	}
}

func TestRemediationAction_ValidStructure(t *testing.T) {
	engine := NewRemediationEngine()

	// Test all default rules have valid action structures
	checkNames := []string{
		"s3_storage",
		"cache_health",
		"memory_usage",
		"disk_space",
		"network_connectivity",
	}

	for _, checkName := range checkNames {
		t.Run(checkName, func(t *testing.T) {
			actions := engine.GetRemediations(checkName)
			if actions == nil {
				t.Fatalf("No actions found for %s", checkName)
			}

			for _, action := range actions {
				// Verify required fields
				if action.ID == "" {
					t.Error("Action ID is required")
				}
				if action.Title == "" {
					t.Error("Action title is required")
				}
				if action.Description == "" {
					t.Error("Action description is required")
				}
				if len(action.Steps) == 0 {
					t.Error("Action must have steps")
				}
				if action.EstimatedTime <= 0 {
					t.Error("Action must have estimated time")
				}
				if action.Impact == "" {
					t.Error("Action must have impact description")
				}
				if action.Category == "" {
					t.Error("Action must have category")
				}

				// Verify automated actions have auto-fix function
				if action.Automated && action.AutoFix == nil {
					t.Errorf("Automated action %s missing AutoFix function", action.ID)
				}
			}
		})
	}
}

func TestRemediationHistory(t *testing.T) {
	engine := NewRemediationEngine()

	// Create multiple remediation attempts
	attempts := []RemediationAttempt{
		{
			ActionID:  "action1",
			CheckName: "check1",
			Timestamp: time.Now(),
			Success:   true,
			Automated: true,
		},
		{
			ActionID:  "action2",
			CheckName: "check2",
			Timestamp: time.Now(),
			Success:   false,
			Error:     errors.New("failed"),
			Automated: false,
		},
	}

	engine.history = attempts

	// Test getting history
	history := engine.GetRemediationHistory(10)
	if len(history) != 2 {
		t.Errorf("Expected 2 history entries, got %d", len(history))
	}

	// Test limiting history
	history = engine.GetRemediationHistory(1)
	if len(history) != 1 {
		t.Errorf("Expected 1 history entry, got %d", len(history))
	}
}

func TestProblemDiagnosis_ConsecutiveFailures(t *testing.T) {
	engine := NewRemediationEngine()

	result := &Result{
		Check:     "test_check",
		Status:    StatusUnhealthy,
		Message:   "Check failed",
		Error:     "test error",
		Timestamp: time.Now(),
	}

	tests := []struct {
		name              string
		consecutiveErrors int
		expectSymptoms    int
	}{
		{
			name:              "Few failures",
			consecutiveErrors: 2,
			expectSymptoms:    1, // Just the error
		},
		{
			name:              "Multiple failures",
			consecutiveErrors: 5,
			expectSymptoms:    2, // Error + consecutive failures symptom
		},
		{
			name:              "Many failures",
			consecutiveErrors: 12,
			expectSymptoms:    3, // Error + consecutive failures + restart warning
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := &pkghealth.ComponentHealth{
				Name:              "test_check",
				ConsecutiveErrors: tt.consecutiveErrors,
			}

			diagnosis := engine.DiagnoseProblem(result, health)

			if len(diagnosis.Symptoms) < tt.expectSymptoms {
				t.Errorf("Expected at least %d symptoms, got %d", tt.expectSymptoms, len(diagnosis.Symptoms))
			}

			if diagnosis.ConsecutiveFailures != tt.consecutiveErrors {
				t.Errorf("Expected %d consecutive failures, got %d", tt.consecutiveErrors, diagnosis.ConsecutiveFailures)
			}
		})
	}
}
