package desktop

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestDesktopContract(t *testing.T) {
	for _, input := range []string{
		`{"command":"status"}`, `{"command":"snapshot","app":"Login","maxDepth":12}`,
		`{"command":"setValue","ref":"@s123:e1","secret":"app.password"}`,
		`{"command":"setValue","ref":"@s123:e1","text":""}`,
		`{"command":"click","ref":"@s123:e1"}`, `{"command":"clear","ref":"@s123:e1"}`,
		`{"command":"cli","args":["list-apps"]}`,
		`{"command":"cli","args":["snapshot","--surface","system-tray"]}`,
		`{"command":"cli","args":["future-upstream-command","--headed"],"stdin":"public input"}`,
	} {
		if _, err := Decode(json.RawMessage(input)); err != nil {
			t.Fatalf("valid command rejected: %v", err)
		}
	}
	for _, input := range []string{
		`null`, `[]`, `{"command":"snapshot"}`, `{"command":"status","secret":"private"}`,
		`{"command":"snapshot","app":"Login","maxDepth":0}`,
		`{"command":"setValue","ref":"@e1","secret":"app.password"}`,
		`{"command":"setValue","ref":"@s123:e1","text":"private","secret":"app.password"}`,
		`{"command":"setValue","ref":"@s123:e1","text":null,"secret":"app.password"}`,
		`{"command":"setValue","ref":"@s123:e1"}`,
		`{"command":"setValue","ref":"@s123:e1","text":"private\u0000value"}`,
		`{"command":"screenshot"}`, `{"command":"clipboard-get"}`, `{"command":"trace"}`,
		`{"command":"click","ref":"@s123:e1","headed":true}`,
		`{"command":"status"} {"text":"private"}`,
		`{"command":"cli","args":[]}`, `{"command":"cli","args":["status"],"secret":"app.password"}`,
		`{"command":"cli","args":["status"],"text":"private"}`,
		`{"command":"cli","args":["private\u0000input"]}`,
		`{"command":"cli","args":["status"],"stdin":"private\u0000input"}`,
		`{"command":"status","args":["unexpected"]}`,
		`{"command":"cli","args":["status"],"stdin":null}`,
	} {
		if _, err := Decode(json.RawMessage(input)); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid command accepted or input echoed")
		}
	}
}

func TestDesktopCLIBoundsAndDiscovery(t *testing.T) {
	for _, args := range [][]string{
		make([]string, 257), {strings.Repeat("x", maxCLIInput+1)}, {strings.Repeat("界", maxCLIInput/2)},
	} {
		input, _ := json.Marshal(Request{Command: "cli", Args: args})
		if _, err := Decode(input); err == nil {
			t.Fatal("CLI argument bounds not enforced")
		}
	}
	args := make([]string, 17)
	for i := range args {
		args[i] = strings.Repeat("x", maxCLIInput/16)
	}
	input, _ := json.Marshal(Request{Command: "cli", Args: args})
	if _, err := Decode(input); err == nil {
		t.Fatal("aggregate CLI argv bound not enforced")
	}
	for _, tc := range []struct {
		native, cli bool
		count       int
	}{{true, false, 5}, {false, true, 1}, {true, true, 6}} {
		if len(SchemaWithCLI(tc.native, tc.cli)["oneOf"].([]any)) != tc.count {
			t.Fatal("schema advertised an unconfigured backend")
		}
	}
}

func TestDesktopPrivatePacket(t *testing.T) {
	text := "pipe-only-秘密"
	r := bytes.NewReader(Packet(Request{Command: "setValue", Ref: "@s123:e1", Text: &text}, "/private/state"))
	magic := make([]byte, 8)
	_, _ = io.ReadFull(r, magic)
	if string(magic) != "CDSK0001" {
		t.Fatal("wrong protocol")
	}
	for _, expected := range []string{"setValue", "", "@s123:e1", text, "/private/state"} {
		var size uint32
		if err := binary.Read(r, binary.BigEndian, &size); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, size)
		if _, err := io.ReadFull(r, b); err != nil || string(b) != expected {
			t.Fatal("packet field mismatch")
		}
	}
	var depth uint32
	if binary.Read(r, binary.BigEndian, &depth) != nil || depth != 10 || r.Len() != 0 {
		t.Fatal("invalid packet boundary or defaults")
	}
}
