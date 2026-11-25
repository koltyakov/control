package model

import (
	"context"
	"errors"
)

// OutcomeNotAccepted means the destination received the request and refused
// it before accepting a task or starting any effect.
const OutcomeNotAccepted = "not_accepted"

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

// ErrReplied is the cause of every error decoded from a destination's reply.
// It shows that the request arrived and the destination answered; whether an
// effect happened still depends on the method. Transport failures, whose
// outcome is uncertain, never carry it.
var ErrReplied = errors.New("the destination replied with an error")

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
	return &RPCError{Code: r.Code, Outcome: r.Outcome, Message: r.Error, Cause: ErrReplied}
}
