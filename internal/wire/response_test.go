package wire

import (
	"bytes"
	"errors"
	"github.com/koltyakov/control/internal/model"
	"strings"
	"testing"
)

func TestOversizedResponseStillHasAReadableOutcome(t *testing.T) {
	response := BoundResponse(model.Response{Result: model.JSON(strings.Repeat("x", MaxFrame))})
	var stream bytes.Buffer
	if err := WriteFrame(&stream, response); err != nil {
		t.Fatal(err)
	}
	var decoded model.Response
	if err := ReadFrame(&stream, &decoded); err != nil {
		t.Fatal(err)
	}
	var rpc *model.RPCError
	if !errors.As(decoded.Err(), &rpc) || rpc.Code != "result_too_large" || rpc.Outcome != "unknown" {
		t.Fatal("lost oversized operation outcome", decoded)
	}
}
