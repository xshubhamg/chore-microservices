// Package apperr defines the single error model shared by every transport.
//
// Domain and application code returns *apperr.Error. The HTTP and gRPC
// transports translate it into a protocol-specific response, so business code
// never imports a web framework or a gRPC status package.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Kind classifies an error so transports can map it without string matching.
type Kind string

const (
	KindInvalid         Kind = "invalid_argument"
	KindUnauthenticated Kind = "unauthenticated"
	KindForbidden       Kind = "permission_denied"
	KindNotFound        Kind = "not_found"
	KindConflict        Kind = "conflict"
	KindUnavailable     Kind = "unavailable"
	KindInternal        Kind = "internal"
)

// Error is a transport-agnostic application error.
//
// Message is safe to return to a client. Cause is retained for logs and never
// serialised.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Details map[string]string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// WithDetail attaches machine-readable context (usually a field name) and
// returns the receiver for chaining.
func (e *Error) WithDetail(key, value string) *Error {
	if e.Details == nil {
		e.Details = make(map[string]string, 1)
	}
	e.Details[key] = value
	return e
}

func New(kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message}
}

func Invalid(code, message string) *Error {
	return New(KindInvalid, code, message)
}

func NotFound(resource string) *Error {
	return New(KindNotFound, "not_found", resource+" not found")
}

func Conflict(code, message string) *Error {
	return New(KindConflict, code, message)
}

func Unavailable(code, message string) *Error {
	return New(KindUnavailable, code, message)
}

// Internal wraps an unexpected error. The cause is logged but never exposed.
func Internal(err error) *Error {
	return &Error{
		Kind:    KindInternal,
		Code:    "internal_error",
		Message: "an unexpected error occurred",
		Cause:   err,
	}
}

// From normalises any error into *Error. Unknown errors become internal errors
// so callers cannot leak implementation details by accident.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return Internal(err)
}

// KindOf returns the kind of err, defaulting to KindInternal.
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	return From(err).Kind
}

// HTTPStatus maps a Kind to an HTTP status code.
func (k Kind) HTTPStatus() int {
	switch k {
	case KindInvalid:
		return http.StatusBadRequest
	case KindUnauthenticated:
		return http.StatusUnauthorized
	case KindForbidden:
		return http.StatusForbidden
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict:
		return http.StatusConflict
	case KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
