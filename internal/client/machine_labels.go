package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/koltyakov/control/internal/model"
)

// ErrMachineLabelsUnsupported means the gateway does not advertise owner
// label management. The change was not sent.
var ErrMachineLabelsUnsupported = errors.New("gateway does not support owner-managed machine labels; upgrade the gateway first (nothing was changed)")

// LabelMachine applies an owner label patch to one machine and returns the
// resulting label view. It checks the gateway's advertisement first: a gateway
// without the feature must never be sent a change it could misread. Errors after
// sending may be uncertain; inspect the machine before another change.
func (c Admin) LabelMachine(ctx context.Context, id string, patch model.LabelPatch) (model.MachineLabels, error) {
	if err := patch.Validate(); err != nil {
		return model.MachineLabels{}, err
	}
	info, err := c.authInfo(ctx, accountTimeout)
	if err != nil {
		return model.MachineLabels{}, err
	}
	if !info.MachineLabels {
		return model.MachineLabels{}, ErrMachineLabelsUnsupported
	}
	var view model.MachineLabels
	if err := c.JSON(ctx, http.MethodPatch, "/v1/fleet/nodes/"+url.PathEscape(id), map[string]model.LabelPatch{"labels": patch}, &view); err != nil {
		return model.MachineLabels{}, err
	}
	return view, nil
}

// ParseLabelChanges reads CLI changes: KEY=VALUE sets an owner value and KEY-
// removes the label, including a node-reported one. reset keys discard the
// owner's entry so the node-reported value applies again.
func ParseLabelChanges(changes, reset []string) (model.LabelPatch, error) {
	patch := model.LabelPatch{}
	for _, change := range changes {
		if key, value, ok := strings.Cut(change, "="); ok {
			if patch.Set == nil {
				patch.Set = map[string]string{}
			}
			if _, duplicate := patch.Set[key]; duplicate {
				return model.LabelPatch{}, fmt.Errorf("label key %q appears more than once", key)
			}
			patch.Set[key] = value
			continue
		}
		if key, ok := strings.CutSuffix(change, "-"); ok {
			patch.Remove = append(patch.Remove, key)
			continue
		}
		return model.LabelPatch{}, fmt.Errorf("label change %q must be KEY=VALUE or KEY-", change)
	}
	patch.Reset = append(patch.Reset, reset...)
	return patch, patch.Validate()
}
