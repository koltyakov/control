package client

import (
	"errors"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

// A refusal before acceptance needs no reconciliation; other errors keep the
// instruction to query the ID first.
func TestTaskSubmitErrorNotAccepted(t *testing.T) {
	refused := &TaskSubmitError{ID: "job", Err: (model.Response{Error: "low disk", Code: "insufficient_disk_space", Outcome: model.OutcomeNotAccepted}).Err()}
	if got := refused.Error(); got != "task job was not accepted: low disk" {
		t.Fatal(got)
	}
	var rpc *model.RPCError
	if !errors.As(refused, &rpc) || rpc.Code != "insufficient_disk_space" || rpc.Outcome != model.OutcomeNotAccepted {
		t.Fatal("refusal lost its code or outcome", rpc)
	}
	uncertain := &TaskSubmitError{ID: "job", Err: (model.Response{Error: "stream reset"}).Err()}
	if got := uncertain.Error(); !strings.Contains(got, "query this ID before retrying") {
		t.Fatal(got)
	}
}
