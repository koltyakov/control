package vaultops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

// Operation names accepted by Handler.Do.
const (
	OpCall          = "call"
	OpMachines      = "machines"
	OpSession       = "session"
	OpForwardStart  = "forward.start"
	OpForwardList   = "forward.list"
	OpForwardStop   = "forward.stop"
	OpTunnelStart   = "tunnel.start"
	OpTunnelList    = "tunnel.list"
	OpTunnelDispose = "tunnel.dispose"
	// OpCheck performs no I/O. The broker dispatches it only under a live
	// grant, so a frontend uses it to gate local work such as reading its
	// own runbook stores.
	OpCheck = "check"
)

// Operations lists every accepted operation name in a stable order.
var Operations = []string{OpCall, OpMachines, OpSession, OpForwardStart, OpForwardList, OpForwardStop, OpTunnelStart, OpTunnelList, OpTunnelDispose, OpCheck}

// CallArgs are the arguments of OpCall. Target is a machine name or stable
// ID; only nodes.list, nodes.select and activities.pool accept an empty one.
// Params must be a JSON object when present.
type CallArgs struct {
	Target string          `json:"target,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// IDArgs identify a forward or retained tunnel.
type IDArgs struct {
	ID string `json:"id"`
}

type emptyArgs struct{}

func (h *Handler) dispatch(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error) {
	if len(args) > MaxArgsBytes {
		return nil, notSent(errors.New("operation arguments exceed the broker limit"))
	}
	switch op {
	case OpCall:
		var q CallArgs
		if err := decode(args, &q); err != nil {
			return nil, err
		}
		params, err := checkCall(q)
		if err != nil {
			return nil, err
		}
		if err := h.runtime.checkAccount(ctx); err != nil {
			return nil, err
		}
		var result json.RawMessage
		if err := h.client.Call(ctx, q.Target, q.Method, params, &result); err != nil {
			return nil, err
		}
		return result, nil
	case OpMachines:
		var q struct {
			Node    *string `json:"node"`
			Compact bool    `json:"compact"`
		}
		if err := decode(args, &q); err != nil {
			return nil, err
		}
		query := client.MachineQuery{Compact: q.Compact}
		if q.Node != nil {
			if *q.Node == "" {
				return nil, notSent(errors.New("node requires a nonempty machine name or stable ID"))
			}
			query.Node = *q.Node
		}
		if err := h.runtime.checkAccount(ctx); err != nil {
			return nil, err
		}
		return h.client.Machines(ctx, query)
	case OpSession:
		if err := decode(args, &emptyArgs{}); err != nil {
			return nil, err
		}
		if err := h.runtime.checkAccount(ctx); err != nil {
			return nil, err
		}
		info, err := h.client.Session(ctx)
		if err != nil {
			return nil, err
		}
		return marshal(info)
	case OpForwardStart:
		var spec client.ForwardSpec
		if err := decode(args, &spec); err != nil {
			return nil, err
		}
		if err := h.runtime.checkAccount(ctx); err != nil {
			return nil, err
		}
		f, err := h.client.StartForward(ctx, spec)
		if err != nil {
			return nil, err
		}
		return marshal(f.Info())
	case OpForwardList:
		if err := decode(args, &emptyArgs{}); err != nil {
			return nil, err
		}
		return marshal(h.client.Forwards())
	case OpForwardStop:
		var q IDArgs
		if err := decode(args, &q); err != nil {
			return nil, err
		}
		if q.ID == "" || len(q.ID) > 128 {
			return nil, notSent(errors.New("forward id is required"))
		}
		if err := h.client.StopForward(q.ID); err != nil {
			return nil, err
		}
		return marshal(map[string]bool{"stopped": true})
	case OpTunnelStart:
		var spec client.PersistentTunnelSpec
		if err := decode(args, &spec); err != nil {
			return nil, err
		}
		if err := client.ValidatePersistentTunnel(spec); err != nil {
			return nil, notSent(err)
		}
		if err := h.runtime.checkAccount(ctx); err != nil {
			return nil, err
		}
		info, err := h.runtime.tunnels.Start(ctx, spec)
		if err != nil {
			return nil, err
		}
		return marshal(info)
	case OpTunnelList:
		if err := decode(args, &emptyArgs{}); err != nil {
			return nil, err
		}
		return marshal(h.runtime.tunnels.List())
	case OpTunnelDispose:
		var q IDArgs
		if err := decode(args, &q); err != nil {
			return nil, err
		}
		if err := client.ValidateTunnelID(q.ID); err != nil {
			return nil, notSent(err)
		}
		if err := h.runtime.tunnels.Dispose(q.ID); err != nil {
			return nil, err
		}
		return marshal(map[string]bool{"disposed": true})
	case OpCheck:
		if err := decode(args, &emptyArgs{}); err != nil {
			return nil, err
		}
		return marshal(map[string]bool{"granted": true})
	default:
		return nil, notSent(fmt.Errorf("unknown broker operation %q", op))
	}
}

func checkCall(q CallArgs) (json.RawMessage, error) {
	if q.Method == "" || len(q.Method) > 256 || len(q.Target) > 256 {
		return nil, notSent(errors.New("call requires a method of at most 256 bytes and a target of at most 256 bytes"))
	}
	for _, r := range q.Method {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !valid {
			return nil, notSent(errors.New("invalid method name"))
		}
	}
	if err := client.ValidateVaultMethod(q.Method); err != nil {
		return nil, notSent(err)
	}
	params := bytes.TrimSpace(q.Params)
	if len(params) == 0 || bytes.Equal(params, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	if params[0] != '{' {
		return nil, notSent(errors.New("call params must be a JSON object"))
	}
	return params, nil
}

// decode strictly reads one JSON object; empty arguments mean {}.
func decode(args json.RawMessage, value any) error {
	data := bytes.TrimSpace(args)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		data = []byte("{}")
	}
	if data[0] != '{' {
		return notSent(errors.New("operation arguments must be a JSON object"))
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return notSent(fmt.Errorf("invalid operation arguments: %w", err))
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return notSent(errors.New("operation arguments must be one JSON object"))
	}
	return nil
}

func marshal(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	return data, err
}

func notSent(err error) error {
	return model.OperationError(err, "not_sent")
}
