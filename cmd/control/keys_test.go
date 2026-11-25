package main

import (
	"maps"
	"strings"
	"testing"
)

func TestParseKeyCreateRole(t *testing.T) {
	for _, test := range []struct {
		args []string
		want map[string]string
	}{
		{[]string{"ci"}, map[string]string{"name": "ci"}},
		{[]string{"ci", "--role", "common"}, map[string]string{"name": "ci"}},
		{[]string{"ci", "--role", "orchestrator"}, map[string]string{"name": "ci", "role": "orchestrator"}},
		{[]string{"--role=orchestrator", "ci"}, map[string]string{"name": "ci", "role": "orchestrator"}},
		{[]string{"-role", "orchestrator", "ci"}, map[string]string{"name": "ci", "role": "orchestrator"}},
	} {
		got, err := parseKeyCreate(test.args)
		if err != nil || !maps.Equal(got, test.want) {
			t.Fatal(test.args, got, err)
		}
	}
	for _, args := range [][]string{
		nil,
		{"--role", "orchestrator"},
		{"ci", "--role"},
		{"ci", "--role="},
		{"ci", "--role", "user"},
		{"ci", "--role", "superuser"},
		{"ci", "--role", "orchestrator", "--role", "common"},
		{"ci", "other"},
		{"ci", "--name", "x"},
	} {
		if got, err := parseKeyCreate(args); err == nil {
			t.Fatal("accepted", args, got)
		}
	}
	if !strings.Contains(fleetUsage, "--role common|orchestrator") || strings.Contains(usage, "--role common|orchestrator") {
		t.Fatal("key role help is not limited to fleet owners")
	}
}
