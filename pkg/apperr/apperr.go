// Package apperr defines machine-readable application errors mapped to HTTP responses.
package apperr

import (
	"errors"
	"net/http"
)

type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
	Err     error  `json:"-"`
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Message + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func (e *Error) WithDetails(d any) *Error {
	c := *e
	c.Details = d
	return &c
}

func (e *Error) Wrap(err error) *Error {
	c := *e
	c.Err = err
	return &c
}

func Validation(details any) *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: "VALIDATION_FAILED", Message: "Request validation failed", Details: details}
}

func BadRequest(message string) *Error {
	return New(http.StatusBadRequest, "BAD_REQUEST", message)
}

func Unauthenticated(message string) *Error {
	return New(http.StatusUnauthorized, "UNAUTHENTICATED", message)
}

func Forbidden(message string) *Error {
	return New(http.StatusForbidden, "FORBIDDEN", message)
}

func NotFound(entity string) *Error {
	return New(http.StatusNotFound, "NOT_FOUND", entity+" not found")
}

func Conflict(code, message string) *Error {
	return New(http.StatusConflict, code, message)
}

func Internal(err error) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: "INTERNAL", Message: "Internal server error", Err: err}
}

func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
