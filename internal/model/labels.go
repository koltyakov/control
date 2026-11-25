package model

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
)

// Owner label bounds. Node-reported labels come from signed registrations and
// are not rewritten; these bounds apply to the fleet owner's overlay.
const (
	MaxOwnerLabels     = 32
	MaxLabelKeyBytes   = 63
	MaxLabelValueBytes = 128
)

var (
	labelKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	labelValuePattern = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{1,128}$`)
)

// ValidLabelKey reports whether an owner may set, remove or reset key. Keys
// start and end with a letter or digit, so "KEY-" is unambiguous on the CLI.
func ValidLabelKey(key string) bool { return labelKeyPattern.MatchString(key) }

// ValidLabelValue reports whether an owner may set value. Empty values are
// rejected because selection treats an absent label as empty.
func ValidLabelValue(value string) bool { return labelValuePattern.MatchString(value) }

// LabelOverlay is a fleet owner's gateway-stored change to the labels a node
// reports. Set values win over node values; Remove hides node-reported keys,
// including ones the node reports only after a later restart.
type LabelOverlay struct {
	Set    map[string]string `json:"set,omitempty"`
	Remove []string          `json:"remove,omitempty"`
}

// Empty reports whether the overlay changes nothing.
func (o LabelOverlay) Empty() bool { return len(o.Set) == 0 && len(o.Remove) == 0 }

// Apply returns the effective labels: reported labels without removed keys,
// then owner values. It never modifies reported.
func (o LabelOverlay) Apply(reported map[string]string) map[string]string {
	if o.Empty() {
		return reported
	}
	effective := maps.Clone(reported)
	if effective == nil {
		effective = map[string]string{}
	}
	for _, key := range o.Remove {
		delete(effective, key)
	}
	maps.Copy(effective, o.Set)
	if len(effective) == 0 {
		return nil
	}
	return effective
}

// LabelPatch changes an overlay. Set records owner values, Remove records
// removals, and Reset discards the owner's entry so the node-reported value,
// if any, applies again. A key may appear in only one list.
type LabelPatch struct {
	Set    map[string]string `json:"set,omitempty"`
	Remove []string          `json:"remove,omitempty"`
	Reset  []string          `json:"reset,omitempty"`
}

// Validate checks bounds and syntax before any state is read.
func (p LabelPatch) Validate() error {
	count := len(p.Set) + len(p.Remove) + len(p.Reset)
	if count == 0 {
		return errors.New("label change requires at least one set, remove or reset entry")
	}
	if count > 2*MaxOwnerLabels {
		return fmt.Errorf("label change has more than %d entries", 2*MaxOwnerLabels)
	}
	seen := map[string]bool{}
	check := func(key string) error {
		if !ValidLabelKey(key) {
			return fmt.Errorf("label key %q must be 1..%d letters, digits, dots, hyphens or underscores, starting and ending with a letter or digit", key, MaxLabelKeyBytes)
		}
		if seen[key] {
			return fmt.Errorf("label key %q appears more than once", key)
		}
		seen[key] = true
		return nil
	}
	for key, value := range p.Set {
		if err := check(key); err != nil {
			return err
		}
		if !ValidLabelValue(value) {
			return fmt.Errorf("label %q value must be 1..%d letters, digits or . _ : / @ + -", key, MaxLabelValueBytes)
		}
	}
	for _, key := range slices.Concat(p.Remove, p.Reset) {
		if err := check(key); err != nil {
			return err
		}
	}
	return nil
}

// Patch applies a validated patch and returns the resulting overlay, or an
// error when it would exceed MaxOwnerLabels entries. o is not modified.
func (o LabelOverlay) Patch(p LabelPatch) (LabelOverlay, error) {
	set := maps.Clone(o.Set)
	if set == nil {
		set = map[string]string{}
	}
	removed := map[string]bool{}
	for _, key := range o.Remove {
		removed[key] = true
	}
	for key, value := range p.Set {
		set[key] = value
		delete(removed, key)
	}
	for _, key := range p.Remove {
		delete(set, key)
		removed[key] = true
	}
	for _, key := range p.Reset {
		delete(set, key)
		delete(removed, key)
	}
	if len(set)+len(removed) > MaxOwnerLabels {
		return o, fmt.Errorf("a machine may have at most %d owner label entries, including removals", MaxOwnerLabels)
	}
	next := LabelOverlay{}
	if len(set) != 0 {
		next.Set = set
	}
	next.Remove = slices.Sorted(maps.Keys(removed))
	if len(next.Remove) == 0 {
		next.Remove = nil
	}
	return next, nil
}

// MachineLabels shows where a machine's labels come from. Labels is the
// effective set used by discovery and selection; NodeLabels is exactly what
// the node's signed registration reports.
type MachineLabels struct {
	Labels        map[string]string `json:"labels"`
	OwnerLabels   map[string]string `json:"ownerLabels"`
	RemovedLabels []string          `json:"removedLabels"`
	NodeLabels    map[string]string `json:"nodeLabels"`
}

// LabelView projects a directory record. Gateways without owner labels omit
// the overlay fields, so every label is then node-reported.
func LabelView(n Node) MachineLabels {
	view := MachineLabels{Labels: n.Labels, OwnerLabels: n.OwnerLabels, RemovedLabels: n.RemovedLabels, NodeLabels: n.NodeLabels}
	if len(n.OwnerLabels) == 0 && len(n.RemovedLabels) == 0 {
		view.NodeLabels = n.Labels
	}
	if view.Labels == nil {
		view.Labels = map[string]string{}
	}
	if view.OwnerLabels == nil {
		view.OwnerLabels = map[string]string{}
	}
	if view.RemovedLabels == nil {
		view.RemovedLabels = []string{}
	}
	if view.NodeLabels == nil {
		view.NodeLabels = map[string]string{}
	}
	return view
}
