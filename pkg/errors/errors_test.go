package errors

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNewError(t *testing.T) {
	t.Parallel()

	t.Run("creates error with all defaults", func(t *testing.T) {
		err := NewError(ErrCodeInvalidConfig, "configuration is invalid")
		if err == nil {
			t.Fatal("NewError returned nil")
		}
		if err.Code != ErrCodeInvalidConfig {
			t.Errorf("Code = %v, want %v", err.Code, ErrCodeInvalidConfig)
		}
		if err.Message != "configuration is invalid" {
			t.Errorf("Message = %q, want %q", err.Message, "configuration is invalid")
		}
		if err.Category != CategoryConfiguration {
			t.Errorf("Category = %v, want %v", err.Category, CategoryConfiguration)
		}
		if err.Details == nil {
			t.Error("Details map is nil")
		}
		if err.Context == nil {
			t.Error("Context map is nil")
		}
		if err.Timestamp.IsZero() {
			t.Error("Timestamp not set")
		}
	})

	t.Run("sets correct retryable defaults", func(t *testing.T) {
		retryableErr := NewError(ErrCodeConnectionTimeout, "connection timed out")
		if !retryableErr.Retryable {
			t.Error("ConnectionTimeout should be retryable by default")
		}

		nonRetryableErr := NewError(ErrCodeInvalidConfig, "config invalid")
		if nonRetryableErr.Retryable {
			t.Error("InvalidConfig should not be retryable by default")
		}
	})

	t.Run("sets correct user-facing defaults", func(t *testing.T) {
		userFacingErr := NewError(ErrCodeFileNotFound, "file not found")
		if !userFacingErr.UserFacing {
			t.Error("FileNotFound should be user-facing by default")
		}

		internalErr := NewError(ErrCodeInternalError, "internal error")
		if internalErr.UserFacing {
			t.Error("InternalError should not be user-facing by default")
		}
	})

	t.Run("sets correct HTTP status defaults", func(t *testing.T) {
		tests := []struct {
			code       ErrorCode
			wantStatus int
		}{
			{ErrCodeInvalidConfig, 400},
			{ErrCodeAuthenticationFailed, 401},
			{ErrCodePermissionDenied, 403},
			{ErrCodeFileNotFound, 404},
			{ErrCodeDirectoryExists, 409},
			{ErrCodeResourceExhausted, 429},
			{ErrCodeInternalError, 500},
			{ErrCodeOperationTimeout, 504},
		}

		for _, tt := range tests {
			err := NewError(tt.code, "test")
			if err.HTTPStatus != tt.wantStatus {
				t.Errorf("%v: HTTPStatus = %d, want %d", tt.code, err.HTTPStatus, tt.wantStatus)
			}
		}
	})
}

func TestGetCategory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code     ErrorCode
		expected ErrorCategory
	}{
		{ErrCodeInvalidConfig, CategoryConfiguration},
		{ErrCodeConfigLoad, CategoryConfiguration},
		{ErrCodeConnectionFailed, CategoryConnection},
		{ErrCodeNetworkError, CategoryConnection},
		{ErrCodeObjectNotFound, CategoryStorage},
		{ErrCodeBucketExists, CategoryStorage},
		{ErrCodeMountFailed, CategoryFilesystem},
		{ErrCodeFileNotFound, CategoryFilesystem},
		{ErrCodeOutOfMemory, CategoryResource},
		{ErrCodeBufferFull, CategoryResource},
		{ErrCodeAlreadyStarted, CategoryState},
		{ErrCodeNotInitialized, CategoryState},
		{ErrCodeOperationTimeout, CategoryOperation},
		{ErrCodeValidationFailed, CategoryOperation},
		{ErrCodeAuthenticationFailed, CategoryAuth},
		{ErrCodeTokenExpired, CategoryAuth},
		{ErrCodeInternalError, CategoryInternal},
		{ErrCodeUnknownError, CategoryInternal},
	}

	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			result := GetCategory(tt.code)
			if result != tt.expected {
				t.Errorf("GetCategory(%v) = %v, want %v", tt.code, result, tt.expected)
			}
		})
	}
}

func TestIsRetryableByDefault(t *testing.T) {
	t.Parallel()

	retryableCodes := []ErrorCode{
		ErrCodeConnectionTimeout,
		ErrCodeConnectionFailed,
		ErrCodeNetworkError,
		ErrCodeOperationTimeout,
		ErrCodeResourceExhausted,
		ErrCodeWorkerBusy,
		ErrCodeInternalError,
	}

	nonRetryableCodes := []ErrorCode{
		ErrCodeInvalidConfig,
		ErrCodeFileNotFound,
		ErrCodePermissionDenied,
		ErrCodeValidationFailed,
	}

	for _, code := range retryableCodes {
		t.Run(string(code)+" should be retryable", func(t *testing.T) {
			if !IsRetryableByDefault(code) {
				t.Errorf("%v should be retryable by default", code)
			}
		})
	}

	for _, code := range nonRetryableCodes {
		t.Run(string(code)+" should not be retryable", func(t *testing.T) {
			if IsRetryableByDefault(code) {
				t.Errorf("%v should not be retryable by default", code)
			}
		})
	}
}

func TestIsUserFacingByDefault(t *testing.T) {
	t.Parallel()

	userFacingCodes := []ErrorCode{
		ErrCodeInvalidConfig,
		ErrCodeMissingConfig,
		ErrCodePermissionDenied,
		ErrCodeFileNotFound,
		ErrCodeMountFailed,
		ErrCodeOperationTimeout,
	}

	internalCodes := []ErrorCode{
		ErrCodeInternalError,
		ErrCodePanicRecovered,
		ErrCodeConnectionPool,
		ErrCodeWorkerBusy,
	}

	for _, code := range userFacingCodes {
		t.Run(string(code)+" should be user-facing", func(t *testing.T) {
			if !IsUserFacingByDefault(code) {
				t.Errorf("%v should be user-facing by default", code)
			}
		})
	}

	for _, code := range internalCodes {
		t.Run(string(code)+" should not be user-facing", func(t *testing.T) {
			if IsUserFacingByDefault(code) {
				t.Errorf("%v should not be user-facing by default", code)
			}
		})
	}
}

func TestGetDefaultHTTPStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code       ErrorCode
		wantStatus int
	}{
		{ErrCodeInvalidConfig, 400},
		{ErrCodePathInvalid, 400},
		{ErrCodeAuthenticationFailed, 401},
		{ErrCodeTokenExpired, 401},
		{ErrCodePermissionDenied, 403},
		{ErrCodeAccessDenied, 403},
		{ErrCodeFileNotFound, 404},
		{ErrCodeObjectNotFound, 404},
		{ErrCodeDirectoryExists, 409},
		{ErrCodeAlreadyStarted, 409},
		{ErrCodeResourceExhausted, 429},
		{ErrCodeQuotaExceeded, 429},
		{ErrCodeInternalError, 500},
		{ErrCodeOperationTimeout, 504},
		{ErrCodeConnectionTimeout, 504},
		// Unmapped code should default to 500
		{ErrorCode("UNKNOWN_CODE"), 500},
	}

	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			result := GetDefaultHTTPStatus(tt.code)
			if result != tt.wantStatus {
				t.Errorf("GetDefaultHTTPStatus(%v) = %d, want %d", tt.code, result, tt.wantStatus)
			}
		})
	}
}

func TestObjectFSError_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *ObjectFSError
		want string
	}{
		{
			name: "with component and operation",
			err: &ObjectFSError{
				Code:      ErrCodeFileNotFound,
				Component: "storage",
				Operation: "read",
				Message:   "file does not exist",
			},
			want: "[storage:read] FILE_NOT_FOUND: file does not exist",
		},
		{
			name: "with component only",
			err: &ObjectFSError{
				Code:      ErrCodeInvalidConfig,
				Component: "config",
				Message:   "invalid value",
			},
			want: "[config] INVALID_CONFIG: invalid value",
		},
		{
			name: "minimal error",
			err: &ObjectFSError{
				Code:    ErrCodeUnknownError,
				Message: "something went wrong",
			},
			want: "UNKNOWN_ERROR: something went wrong",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.err.Error()
			if result != tt.want {
				t.Errorf("Error() = %q, want %q", result, tt.want)
			}
		})
	}
}

func TestObjectFSError_Unwrap(t *testing.T) {
	t.Parallel()

	cause := errors.New("underlying cause")
	err := &ObjectFSError{
		Code:    ErrCodeInternalError,
		Message: "wrapper",
		Cause:   cause,
	}

	unwrapped := err.Unwrap()
	if unwrapped != cause {
		t.Errorf("Unwrap() = %v, want %v", unwrapped, cause)
	}
}

func TestObjectFSError_Is(t *testing.T) {
	t.Parallel()

	err1 := &ObjectFSError{Code: ErrCodeFileNotFound, Message: "not found"}
	err2 := &ObjectFSError{Code: ErrCodeFileNotFound, Message: "different message"}
	err3 := &ObjectFSError{Code: ErrCodeInvalidConfig, Message: "invalid"}
	stdErr := errors.New("standard error")

	if !err1.Is(err2) {
		t.Error("errors with same code should match with Is()")
	}

	if err1.Is(err3) {
		t.Error("errors with different codes should not match with Is()")
	}

	if err1.Is(stdErr) {
		t.Error("ObjectFSError should not match standard error with Is()")
	}
}

func TestObjectFSError_String(t *testing.T) {
	t.Parallel()

	err := &ObjectFSError{
		Code:      ErrCodeOperationTimeout,
		Category:  CategoryOperation,
		Message:   "operation took too long",
		Component: "backend",
		Operation: "fetch",
		RequestID: "req-123",
		Retryable: true,
		Details:   map[string]interface{}{"duration": 30},
		Cause:     errors.New("network timeout"),
	}

	result := err.String()

	// Check for key components
	expectedParts := []string{
		"Code=OPERATION_TIMEOUT",
		"Category=operation",
		`Message="operation took too long"`,
		"Component=backend",
		"Operation=fetch",
		"RequestID=req-123",
		"Retryable=true",
		"Details=",
		"Cause=",
	}

	for _, part := range expectedParts {
		if !strings.Contains(result, part) {
			t.Errorf("String() missing expected part: %q\nGot: %s", part, result)
		}
	}
}

func TestObjectFSError_JSON(t *testing.T) {
	t.Parallel()

	err := &ObjectFSError{
		Code:       ErrCodeInvalidConfig,
		Category:   CategoryConfiguration,
		Message:    "invalid setting",
		Component:  "config",
		HTTPStatus: 400,
		Retryable:  false,
		UserFacing: true,
	}

	jsonStr := err.JSON()

	// Parse JSON to verify it's valid
	var parsed map[string]interface{}
	if parseErr := json.Unmarshal([]byte(jsonStr), &parsed); parseErr != nil {
		t.Fatalf("JSON() returned invalid JSON: %v\nJSON: %s", parseErr, jsonStr)
	}

	// Check key fields
	if parsed["code"] != "INVALID_CONFIG" {
		t.Errorf("JSON code = %v, want INVALID_CONFIG", parsed["code"])
	}
	if parsed["message"] != "invalid setting" {
		t.Errorf("JSON message = %v, want 'invalid setting'", parsed["message"])
	}
	if parsed["retryable"] != false {
		t.Errorf("JSON retryable = %v, want false", parsed["retryable"])
	}
}

func TestCaptureStack(t *testing.T) {
	t.Parallel()

	stack := CaptureStack(0)

	if stack == "" {
		t.Error("CaptureStack() returned empty string")
	}

	// Stack should contain file paths and line numbers
	if !strings.Contains(stack, ":") {
		t.Error("Stack trace should contain file:line format")
	}

	// Should not include errors.go itself
	if strings.Contains(stack, "errors.go") {
		t.Error("Stack trace should not include errors.go frames")
	}
}

func TestErrorCodeCategories(t *testing.T) {
	t.Parallel()

	// Test that all defined error codes have proper categories
	allCodes := []ErrorCode{
		// Configuration
		ErrCodeInvalidConfig, ErrCodeMissingConfig, ErrCodeConfigValidation,
		// Connection
		ErrCodeConnectionFailed, ErrCodeConnectionTimeout, ErrCodeNetworkError,
		// Storage
		ErrCodeObjectNotFound, ErrCodeBucketNotFound, ErrCodeAccessDenied,
		// Filesystem
		ErrCodeMountFailed, ErrCodeFileNotFound, ErrCodePermissionDenied,
		// Resource
		ErrCodeOutOfMemory, ErrCodeBufferFull, ErrCodeResourceExhausted,
		// State
		ErrCodeAlreadyStarted, ErrCodeNotInitialized, ErrCodeInvalidState,
		// Operation
		ErrCodeOperationTimeout, ErrCodeValidationFailed, ErrCodeRetryExhausted,
		// Auth
		ErrCodeAuthenticationFailed, ErrCodeTokenExpired, ErrCodeCredentialsMissing,
		// Internal
		ErrCodeInternalError, ErrCodePanicRecovered, ErrCodeUnknownError,
	}

	for _, code := range allCodes {
		category := GetCategory(code)
		if category == "" {
			t.Errorf("GetCategory(%v) returned empty category", code)
		}
	}
}
