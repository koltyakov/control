package model

import (
	"context"
	"errors"
)

// RPCError describes delivery certainty, never permission to replay a request.
type RPCError struct {
	Code    string `json:"code"`
	Outcome string `json:"outcome"`
	Message string `json:"message"`
	Cause   error  `json:"-"`
}

func (e *RPCError) Error() string { return e.Message }
func (e *RPCError) Unwrap() error { return e.Cause }

func OperationError(err error, outcome string) *RPCError {
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return rpc
	}
	code := "operation_failed"
	if errors.Is(err, context.Canceled) {
		code = "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		code = "deadline_exceeded"
	}
	return &RPCError{Code: code, Outcome: outcome, Message: err.Error(), Cause: err}
}

func ErrorResponse(err error) Response {
	e := OperationError(err, "unknown")
	return Response{Error: e.Message, Code: e.Code, Outcome: e.Outcome}
}

func (r Response) Err() error {
	if r.Error == "" {
		return nil
	}
	if r.Code == "" {
		r.Code = "remote_error"
	}
	if r.Outcome == "" {
		r.Outcome = "unknown"
	}
	return &RPCError{Code: r.Code, Outcome: r.Outcome, Message: r.Error}
}
