package model

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

// A destination's error reply is distinguishable from a transport failure,
// without changing the message, code or outcome callers already read.
func TestReplyErrorsAreMarked(t *testing.T) {
	reply := Response{Error: "mkdirat dest: file exists", Code: "operation_failed", Outcome: "unknown"}.Err()
	var rpc *RPCError
	if !errors.As(reply, &rpc) || rpc.Message != "mkdirat dest: file exists" || rpc.Code != "operation_failed" || rpc.Outcome != "unknown" {
		t.Fatal("reply fields changed", reply)
	}
	if reply.Error() != "mkdirat dest: file exists" || !errors.Is(fmt.Errorf("wrapped: %w", reply), ErrReplied) {
		t.Fatal("reply not marked", reply)
	}
	if OperationError(reply, "not_sent") != rpc {
		t.Fatal("classifying a reply replaced it")
	}
	for _, transport := range []error{OperationError(io.EOF, "unknown"), OperationError(errors.New("dial"), "not_sent"), &RPCError{Code: "upload_busy", Message: "local"}} {
		if errors.Is(transport, ErrReplied) {
			t.Fatal("non-reply error marked as a reply", transport)
		}
	}
}
