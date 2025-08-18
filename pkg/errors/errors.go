// Package errors provides a structured error system for ObjectFS with error codes, categories, and context.
package errors

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// ErrorCode represents a structured error code for ObjectFS operations.
type ErrorCode string

// Error code constants organized by category with numeric prefixes for sorting.
const (
	// Configuration Errors (1000-1999)
	ErrCodeInvalidConfig    ErrorCode = "INVALID_CONFIG"
	ErrCodeMissingConfig    ErrorCode = "MISSING_CONFIG"
	ErrCodeConfigValidation ErrorCode = "CONFIG_VALIDATION"
	ErrCodeConfigLoad       ErrorCode = "CONFIG_LOAD"
	ErrCodeConfigSave       ErrorCode = "CONFIG_SAVE"

	// Connection Errors (2000-2999)
	ErrCodeConnectionFailed  ErrorCode = "CONNECTION_FAILED"
	ErrCodeConnectionTimeout ErrorCode = "CONNECTION_TIMEOUT"
	ErrCodeConnectionPool    ErrorCode = "CONNECTION_POOL"
	ErrCodeConnectionRefused ErrorCode = "CONNECTION_REFUSED"
	ErrCodeNetworkError      ErrorCode = "NETWORK_ERROR"

	// Storage Backend Errors (3000-3999)
	ErrCodeObjectNotFound ErrorCode = "OBJECT_NOT_FOUND"
	ErrCodeBucketNotFound ErrorCode = "BUCKET_NOT_FOUND"
	ErrCodeStorageWrite   ErrorCode = "STORAGE_WRITE"
	ErrCodeStorageRead    ErrorCode = "STORAGE_READ"
	ErrCodeTierValidation ErrorCode = "TIER_VALIDATION"
	ErrCodeAccessDenied   ErrorCode = "ACCESS_DENIED"
	ErrCodeQuotaExceeded  ErrorCode = "QUOTA_EXCEEDED"
	ErrCodeBucketExists   ErrorCode = "BUCKET_EXISTS"

	// Filesystem Errors (4000-4999)
	ErrCodeMountFailed      ErrorCode = "MOUNT_FAILED"
	ErrCodeUnmountFailed    ErrorCode = "UNMOUNT_FAILED"
	ErrCodePermissionDenied ErrorCode = "PERMISSION_DENIED"
	ErrCodePathInvalid      ErrorCode = "PATH_INVALID"
	ErrCodeFileNotFound     ErrorCode = "FILE_NOT_FOUND"
	ErrCodeDirectoryExists  ErrorCode = "DIRECTORY_EXISTS"
	ErrCodeNotDirectory     ErrorCode = "NOT_DIRECTORY"
	ErrCodeNotEmpty         ErrorCode = "NOT_EMPTY"

	// Resource Management Errors (5000-5999)
	ErrCodeOutOfMemory       ErrorCode = "OUT_OF_MEMORY"
	ErrCodeBufferFull        ErrorCode = "BUFFER_FULL"
	ErrCodeResourceExhausted ErrorCode = "RESOURCE_EXHAUSTED"
	ErrCodeCacheFull         ErrorCode = "CACHE_FULL"
	ErrCodeWorkerBusy        ErrorCode = "WORKER_BUSY"
	ErrCodeLimitExceeded     ErrorCode = "LIMIT_EXCEEDED"

	// State Management Errors (6000-6999)
	ErrCodeAlreadyStarted     ErrorCode = "ALREADY_STARTED"
	ErrCodeNotInitialized     ErrorCode = "NOT_INITIALIZED"
	ErrCodeInvalidState       ErrorCode = "INVALID_STATE"
	ErrCodeShutdownInProgress ErrorCode = "SHUTDOWN_IN_PROGRESS"
	ErrCodeComponentStopped   ErrorCode = "COMPONENT_STOPPED"

	// Operation Errors (7000-7999)
	ErrCodeOperationTimeout  ErrorCode = "OPERATION_TIMEOUT"
	ErrCodeOperationCanceled ErrorCode = "OPERATION_CANCELED"
	ErrCodeOperationFailed   ErrorCode = "OPERATION_FAILED"
	ErrCodeRetryExhausted    ErrorCode = "RETRY_EXHAUSTED"
	ErrCodeValidationFailed  ErrorCode = "VALIDATION_FAILED"

	// Authentication/Authorization Errors (8000-8999)
	ErrCodeAuthenticationFailed ErrorCode = "AUTHENTICATION_FAILED"
	ErrCodeAuthorizationFailed  ErrorCode = "AUTHORIZATION_FAILED"
	ErrCodeTokenExpired         ErrorCode = "TOKEN_EXPIRED"
	ErrCodeCredentialsMissing   ErrorCode = "CREDENTIALS_MISSING"

	// Internal System Errors (9000-9999)
	ErrCodeInternalError  ErrorCode = "INTERNAL_ERROR"
	ErrCodePanicRecovered ErrorCode = "PANIC_RECOVERED"
	ErrCodeUnknownError   ErrorCode = "UNKNOWN_ERROR"
)

// ErrorCategory represents the general category of an error.
type ErrorCategory string

const (
	CategoryConfiguration ErrorCategory = "configuration"
	CategoryConnection    ErrorCategory = "connection"
	CategoryStorage       ErrorCategory = "storage"
	CategoryFilesystem    ErrorCategory = "filesystem"
	CategoryResource      ErrorCategory = "resource"
	CategoryState         ErrorCategory = "state"
	CategoryOperation     ErrorCategory = "operation"
	CategoryAuth          ErrorCategory = "auth"
	CategoryInternal      ErrorCategory = "internal"
)

// ObjectFSError represents a structured error with context and metadata.
type ObjectFSError struct {
	// Core error information
	Code     ErrorCode              `json:"code"`
	Category ErrorCategory          `json:"category"`
	Message  string                 `json:"message"`
	Details  map[string]interface{} `json:"details,omitempty"`

	// Contextual information
	Context   map[string]string `json:"context,omitempty"`
	Cause     error             `json:"-"` // Not serialized to avoid circular refs
	Timestamp time.Time         `json:"timestamp"`

	// Operational metadata
	Component string `json:"component"`
	Operation string `json:"operation,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`

	// Error handling hints
	Retryable  bool `json:"retryable"`
	UserFacing bool `json:"user_facing"`
	HTTPStatus int  `json:"http_status,omitempty"`

	// Debug information
	Stack string `json:"stack,omitempty"`
}

// Error implements the error interface.
func (e *ObjectFSError) Error() string {
	if e.Component != "" {
		if e.Operation != "" {
			return fmt.Sprintf("[%s:%s] %s: %s", e.Component, e.Operation, e.Code, e.Message)
		}
		return fmt.Sprintf("[%s] %s: %s", e.Component, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying cause error for error wrapping compatibility.
func (e *ObjectFSError) Unwrap() error {
	return e.Cause
}

// Is checks if the error matches the target error (for errors.Is compatibility).
func (e *ObjectFSError) Is(target error) bool {
	if objectFSErr, ok := target.(*ObjectFSError); ok {
		return e.Code == objectFSErr.Code
	}
	return false
}

// String returns a detailed string representation for logging.
func (e *ObjectFSError) String() string {
	var parts []string

	parts = append(parts, fmt.Sprintf("Code=%s", e.Code))
	parts = append(parts, fmt.Sprintf("Category=%s", e.Category))
	parts = append(parts, fmt.Sprintf("Message=%q", e.Message))

	if e.Component != "" {
		parts = append(parts, fmt.Sprintf("Component=%s", e.Component))
	}

	if e.Operation != "" {
		parts = append(parts, fmt.Sprintf("Operation=%s", e.Operation))
	}

	if e.RequestID != "" {
		parts = append(parts, fmt.Sprintf("RequestID=%s", e.RequestID))
	}

	if e.Retryable {
		parts = append(parts, "Retryable=true")
	}

	if len(e.Details) > 0 {
		details, _ := json.Marshal(e.Details)
		parts = append(parts, fmt.Sprintf("Details=%s", details))
	}

	if e.Cause != nil {
		parts = append(parts, fmt.Sprintf("Cause=%q", e.Cause.Error()))
	}

	return fmt.Sprintf("ObjectFSError{%s}", strings.Join(parts, ", "))
}

// JSON returns the error as a JSON string.
func (e *ObjectFSError) JSON() string {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Sprintf(`{"error":"failed to marshal error: %s"}`, err.Error())
	}
	return string(data)
}

// NewError creates a new ObjectFS error with default values.
func NewError(code ErrorCode, message string) *ObjectFSError {
	return &ObjectFSError{
		Code:       code,
		Category:   GetCategory(code),
		Message:    message,
		Timestamp:  time.Now(),
		Details:    make(map[string]interface{}),
		Context:    make(map[string]string),
		Retryable:  IsRetryableByDefault(code),
		UserFacing: IsUserFacingByDefault(code),
		HTTPStatus: GetDefaultHTTPStatus(code),
	}
}

// GetCategory determines the category based on the error code.
func GetCategory(code ErrorCode) ErrorCategory {
	codeStr := string(code)
	switch {
	case strings.HasPrefix(codeStr, "INVALID_CONFIG") || strings.HasPrefix(codeStr, "MISSING_CONFIG") ||
		strings.HasPrefix(codeStr, "CONFIG_"):
		return CategoryConfiguration
	case strings.HasPrefix(codeStr, "CONNECTION_") || strings.HasPrefix(codeStr, "NETWORK_"):
		return CategoryConnection
	case strings.HasPrefix(codeStr, "OBJECT_") || strings.HasPrefix(codeStr, "BUCKET_") ||
		strings.HasPrefix(codeStr, "STORAGE_") || strings.HasPrefix(codeStr, "TIER_") ||
		strings.HasPrefix(codeStr, "ACCESS_") || strings.HasPrefix(codeStr, "QUOTA_"):
		return CategoryStorage
	case strings.HasPrefix(codeStr, "MOUNT_") || strings.HasPrefix(codeStr, "UNMOUNT_") ||
		strings.HasPrefix(codeStr, "PERMISSION_") || strings.HasPrefix(codeStr, "PATH_") ||
		strings.HasPrefix(codeStr, "FILE_") || strings.HasPrefix(codeStr, "DIRECTORY_"):
		return CategoryFilesystem
	case strings.HasPrefix(codeStr, "OUT_OF_") || strings.HasPrefix(codeStr, "BUFFER_") ||
		strings.HasPrefix(codeStr, "RESOURCE_") || strings.HasPrefix(codeStr, "CACHE_") ||
		strings.HasPrefix(codeStr, "WORKER_") || strings.HasPrefix(codeStr, "LIMIT_"):
		return CategoryResource
	case strings.HasPrefix(codeStr, "ALREADY_") || strings.HasPrefix(codeStr, "NOT_INITIALIZED") ||
		strings.HasPrefix(codeStr, "INVALID_STATE") || strings.HasPrefix(codeStr, "SHUTDOWN_") ||
		strings.HasPrefix(codeStr, "COMPONENT_"):
		return CategoryState
	case strings.HasPrefix(codeStr, "OPERATION_") || strings.HasPrefix(codeStr, "RETRY_") ||
		strings.HasPrefix(codeStr, "VALIDATION_"):
		return CategoryOperation
	case strings.HasPrefix(codeStr, "AUTHENTICATION_") || strings.HasPrefix(codeStr, "AUTHORIZATION_") ||
		strings.HasPrefix(codeStr, "TOKEN_") || strings.HasPrefix(codeStr, "CREDENTIALS_"):
		return CategoryAuth
	default:
		return CategoryInternal
	}
}

// IsRetryableByDefault determines if an error is retryable by default.
func IsRetryableByDefault(code ErrorCode) bool {
	retryableCodes := map[ErrorCode]bool{
		ErrCodeConnectionTimeout: true,
		ErrCodeConnectionFailed:  true,
		ErrCodeNetworkError:      true,
		ErrCodeOperationTimeout:  true,
		ErrCodeResourceExhausted: true,
		ErrCodeWorkerBusy:        true,
		ErrCodeInternalError:     true,
	}
	return retryableCodes[code]
}

// IsUserFacingByDefault determines if an error should be shown to users.
func IsUserFacingByDefault(code ErrorCode) bool {
	userFacingCodes := map[ErrorCode]bool{
		ErrCodeInvalidConfig:    true,
		ErrCodeMissingConfig:    true,
		ErrCodeConfigValidation: true,
		ErrCodePermissionDenied: true,
		ErrCodePathInvalid:      true,
		ErrCodeFileNotFound:     true,
		ErrCodeAccessDenied:     true,
		ErrCodeMountFailed:      true,
		ErrCodeOperationTimeout: true,
		ErrCodeValidationFailed: true,
	}
	return userFacingCodes[code]
}

// GetDefaultHTTPStatus returns the default HTTP status for an error code.
func GetDefaultHTTPStatus(code ErrorCode) int {
	statusMap := map[ErrorCode]int{
		ErrCodeInvalidConfig:        400, // Bad Request
		ErrCodeConfigValidation:     400,
		ErrCodePathInvalid:          400,
		ErrCodeValidationFailed:     400,
		ErrCodeAuthenticationFailed: 401, // Unauthorized
		ErrCodeCredentialsMissing:   401,
		ErrCodeTokenExpired:         401,
		ErrCodePermissionDenied:     403, // Forbidden
		ErrCodeAuthorizationFailed:  403,
		ErrCodeAccessDenied:         403,
		ErrCodeFileNotFound:         404, // Not Found
		ErrCodeObjectNotFound:       404,
		ErrCodeBucketNotFound:       404,
		ErrCodeDirectoryExists:      409, // Conflict
		ErrCodeBucketExists:         409,
		ErrCodeAlreadyStarted:       409,
		ErrCodeResourceExhausted:    429, // Too Many Requests
		ErrCodeLimitExceeded:        429,
		ErrCodeQuotaExceeded:        429,
		ErrCodeInternalError:        500, // Internal Server Error
		ErrCodeOperationTimeout:     504, // Gateway Timeout
		ErrCodeConnectionTimeout:    504,
	}

	if status, ok := statusMap[code]; ok {
		return status
	}
	return 500 // Default to Internal Server Error
}

// CaptureStack captures the current stack trace for debugging.
func CaptureStack(skip int) string {
	const depth = 10
	var pcs [depth]uintptr
	n := runtime.Callers(skip+2, pcs[:]) // +2 to skip this function and the caller
	frames := runtime.CallersFrames(pcs[:n])

	var stack []string
	for {
		frame, more := frames.Next()
		if !strings.Contains(frame.File, "errors.go") { // Skip frames from this file
			stack = append(stack, fmt.Sprintf("%s:%d %s", frame.File, frame.Line, frame.Function))
		}
		if !more {
			break
		}
	}
	return strings.Join(stack, "\n")
}
