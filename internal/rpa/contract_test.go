package rpa

import (
	"encoding/json"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"inspection", `{"actions":[{"type":"inspect","limit":200},{"type":"screenshot"}]}`, true},
		{"coordinates", `{"actions":[{"type":"click","x":12,"y":30,"count":2,"button":"right"}]}`, true},
		{"selector", `{"actions":[{"type":"click","target":{"app":"Editor","name":"Save","role":"Button"}}]}`, true},
		{"unicode value", `{"actions":[{"type":"setValue","target":{"name":"Search"},"text":"世界"}]}`, true},
		{"secret value", `{"actions":[{"type":"setValue","target":{"name":"Password"},"secret":"app.password"},{"type":"type","secret":"app.user"}]}`, true},
		{"secret and text", `{"actions":[{"type":"type","secret":"password","text":"literal"}]}`, false},
		{"missing value", `{"actions":[{"type":"type"}]}`, false},
		{"invalid secret name", `{"actions":[{"type":"type","secret":"../password"}]}`, false},
		{"secret on inspect", `{"actions":[{"type":"inspect","secret":"password"}]}`, false},
		{"empty", `{"actions":[]}`, false},
		{"null", `null`, false},
		{"unknown", `{"actions":[{"type":"shell","command":"whoami"}]}`, false},
		{"later invalid", `{"actions":[{"type":"click","x":1,"y":2},{"type":"wait","milliseconds":5001}]}`, false},
		{"missing y", `{"actions":[{"type":"click","x":1}]}`, false},
		{"mixed target", `{"actions":[{"type":"click","target":{"name":"Save"},"x":1,"y":2}]}`, false},
		{"double selector", `{"actions":[{"type":"click","target":{"name":"Save"},"count":2}]}`, false},
		{"empty selector", `{"actions":[{"type":"focus","target":{}}]}`, false},
		{"empty selector value", `{"actions":[{"type":"focus","target":{"name":""}}]}`, false},
		{"screenshot path", `{"actions":[{"type":"screenshot","path":"../secret"}]}`, false},
		{"fractional coordinate", `{"actions":[{"type":"move","x":1.5,"y":2}]}`, false},
		{"unknown field", `{"actions":[{"type":"inspect"}],"command":"other"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(json.RawMessage(tc.input)); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
