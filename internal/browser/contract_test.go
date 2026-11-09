package browser

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	for _, raw := range []string{
		`{"args":["snapshot","-i"]}`,
		`{"session":"work-1","args":["fill","@e1","literal ; $(no-shell)"],"stdin":"public"}`,
	} {
		if _, err := Decode(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{
		`null`, `{}`, `{"args":[]}`, `{"args":[1]}`, `{"args":[null]}`, `{"args":["open"],"session":null}`, `{"args":["open"],"unknown":"private"}`,
		`{"args":["open"]} {}`, `{"session":"../other","args":["open"]}`,
		`{"args":["fill","\u0000private"]}`, `{"args":["open"],"stdin":"\u0000private"}`,
		`{"args":["` + strings.Repeat("x", MaxInput+1) + `"]}`,
		`{"args":["` + strings.Repeat("x", MaxInput) + `"],"stdin":"x"}`,
		`{"args":[` + strings.Repeat(`"x",`, 256) + `"x"]}`,
	} {
		if _, err := Decode(json.RawMessage(raw)); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid request accepted or input exposed")
		}
	}
}
