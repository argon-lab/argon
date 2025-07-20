package wal

import (
	"errors"
	"fmt"
)

// WAL-specific error types for better error handling and recovery
var (
	// Core WAL errors
	ErrInvalidLSN       = errors.New("invalid LSN")
	ErrLSNOutOfRange    = errors.New("LSN out of range")
	ErrInvalidOperation = errors.New("invalid operation")
	ErrInvalidEntry     = errors.New("invalid WAL entry")
	
	// Connection and database errors
	ErrDatabaseConnection = errors.New("database connection failed")
	ErrDatabaseTimeout    = errors.New("database operation timeout")
	ErrDatabaseUnavailable = errors.New("database temporarily unavailable")
	
	// Branch and project errors
	ErrBranchNotFound     = errors.New("branch not found")
	ErrProjectNotFound    = errors.New("project not found")
	ErrBranchExists       = errors.New("branch already exists")
	ErrProjectExists      = errors.New("project already exists")
	ErrInvalidBranchState = errors.New("invalid branch state")
	
	// Time travel errors
	ErrTimeTravelFailed    = errors.New("time travel operation failed")
	ErrInvalidTimestamp    = errors.New("invalid timestamp")
	ErrNoEntriesFound      = errors.New("no entries found for specified criteria")
	ErrMaterializationFailed = errors.New("state materialization failed")
	
	// Restore operation errors
	ErrRestoreFailed       = errors.New("restore operation failed")
	ErrUnsafeRestore       = errors.New("restore would cause data loss")
	ErrRestoreValidation   = errors.New("restore validation failed")
	ErrIncompatibleRestore = errors.New("incompatible restore operation")
)

// WALError provides structured error information
type WALError struct {
	Type      string            `json:"type"`
	Message   string            `json:"message"`
	Details   map[string]interface{} `json:"details,omitempty"`
	Cause     error             `json:"-"`
	Retryable bool              `json:"retryable"`
}

func (e *WALError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *WALError) Unwrap() error {
	return e.Cause
}

// NewWALError creates a new structured WAL error
func NewWALError(errorType, message string, cause error, retryable bool) *WALError {
	return &WALError{
		Type:      errorType,
		Message:   message,
		Cause:     cause,
		Retryable: retryable,
		Details:   make(map[string]interface{}),
	}
}

// WithDetail adds detail information to the error
func (e *WALError) WithDetail(key string, value interface{}) *WALError {
	if e.Details == nil {
		e.Details = make(map[string]interface{})
	}
	e.Details[key] = value
	return e
}

// IsRetryable checks if an error is retryable
func IsRetryable(err error) bool {
	var walErr *WALError
	if errors.As(err, &walErr) {
		return walErr.Retryable
	}
	return false
}

// IsType checks if an error is of a specific type
func IsType(err error, errorType string) bool {
	var walErr *WALError
	if errors.As(err, &walErr) {
		return walErr.Type == errorType
	}
	return false
}

// Common error types
const (
	ErrorTypeValidation   = "validation"
	ErrorTypeDatabase     = "database"
	ErrorTypeTimeout      = "timeout"
	ErrorTypeNotFound     = "not_found"
	ErrorTypeConflict     = "conflict"
	ErrorTypeInternal     = "internal"
	ErrorTypePermission   = "permission"
	ErrorTypeRateLimit    = "rate_limit"
)

// Validation errors
func NewValidationError(message string, details map[string]interface{}) *WALError {
	err := NewWALError(ErrorTypeValidation, message, nil, false)
	for k, v := range details {
		err.WithDetail(k, v)
	}
	return err
}

// Database errors (retryable)
func NewDatabaseError(message string, cause error) *WALError {
	return NewWALError(ErrorTypeDatabase, message, cause, true)
}

// Timeout errors (retryable)
func NewTimeoutError(message string, cause error) *WALError {
	return NewWALError(ErrorTypeTimeout, message, cause, true)
}

// Not found errors (not retryable)
func NewNotFoundError(resource string, id string) *WALError {
	return NewWALError(ErrorTypeNotFound, fmt.Sprintf("%s not found", resource), nil, false).
		WithDetail("resource", resource).
		WithDetail("id", id)
}

// Conflict errors (not retryable without user intervention)
func NewConflictError(message string, details map[string]interface{}) *WALError {
	err := NewWALError(ErrorTypeConflict, message, nil, false)
	for k, v := range details {
		err.WithDetail(k, v)
	}
	return err
}

// Recovery helpers
func ShouldRetry(err error, attempt int, maxAttempts int) bool {
	if attempt >= maxAttempts {
		return false
	}
	return IsRetryable(err)
}

// Error context for debugging
type ErrorContext struct {
	Operation string            `json:"operation"`
	LSN       int64             `json:"lsn,omitempty"`
	ProjectID string            `json:"project_id,omitempty"`
	BranchID  string            `json:"branch_id,omitempty"`
	Details   map[string]interface{} `json:"details,omitempty"`
}

func NewErrorContext(operation string) *ErrorContext {
	return &ErrorContext{
		Operation: operation,
		Details:   make(map[string]interface{}),
	}
}

func (ctx *ErrorContext) WithLSN(lsn int64) *ErrorContext {
	ctx.LSN = lsn
	return ctx
}

func (ctx *ErrorContext) WithProject(projectID string) *ErrorContext {
	ctx.ProjectID = projectID
	return ctx
}

func (ctx *ErrorContext) WithBranch(branchID string) *ErrorContext {
	ctx.BranchID = branchID
	return ctx
}

func (ctx *ErrorContext) WithDetail(key string, value interface{}) *ErrorContext {
	ctx.Details[key] = value
	return ctx
}