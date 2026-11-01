package desktop

import (
	"bytes"
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopReadPacket(t *testing.T) {
	home := t.TempDir()
	text := "private-pipe-\"credential-秘密"
	for _, r := range []Request{
		{Command: "status"}, {Command: "snapshot", App: "Test app", MaxDepth: 32},
		{Command: "setValue", Ref: "@stest:e1", Text: &text},
		{Command: "click", Ref: "@stest:e1"}, {Command: "clear", Ref: "@stest:e1"},
	} {
		got, root, err := ReadPacket(bytes.NewReader(Packet(r, home)))
		if err != nil || root != home || got.Command != r.Command || got.App != r.App || got.Ref != r.Ref || got.Secret != "" {
			t.Fatal("packet did not round-trip")
		}
		if r.Text != nil && (got.Text == nil || *got.Text != text) {
			t.Fatal("pipe text changed")
		}
		if r.Command == "snapshot" && got.MaxDepth != 32 {
			t.Fatal("depth changed")
		}
	}
}

func TestDesktopHelperRejectsPacketsPrivately(t *testing.T) {
	home := t.TempDir()
	canary := "private-path-input-panic-canary"
	valid := Packet(Request{Command: "status"}, home)
	badMagic := bytes.Clone(valid)
	badMagic[0] = 'X'
	oversized := bytes.Clone(valid)
	binary.BigEndian.PutUint32(oversized[8:12], ^uint32(0))
	badDepth := bytes.Clone(valid)
	binary.BigEndian.PutUint32(badDepth[len(badDepth)-4:], 33)
	zeroDepth := bytes.Clone(valid)
	clear(zeroDepth[len(zeroDepth)-4:])
	badUTF8 := string([]byte{0xff})
	longText := strings.Repeat("x", 4097)
	nulText := canary + "\x00"
	for _, packet := range [][]byte{
		nil, []byte(canary), badMagic, oversized, badDepth, zeroDepth, append(bytes.Clone(valid), 'x'),
		Packet(Request{Command: "screenshot"}, home), Packet(Request{Command: "status", App: canary}, home),
		Packet(Request{Command: "status"}, "relative"), Packet(Request{Command: "status"}, home+"\x00"),
		Packet(Request{Command: "snapshot", App: badUTF8}, home), Packet(Request{Command: "snapshot"}, home),
		Packet(Request{Command: "snapshot", App: longText}, home),
		Packet(Request{Command: "status"}, home+strings.Repeat("x", 32769)),
		Packet(Request{Command: "snapshot", App: "app", Ref: "@stest:e1"}, home),
		Packet(Request{Command: "setValue", Ref: "unqualified", Text: &canary}, home),
		Packet(Request{Command: "setValue", Ref: "@stest:e1", Text: &longText}, home),
		Packet(Request{Command: "setValue", Ref: "@stest:e1", Text: &nulText}, home),
		Packet(Request{Command: "setValue", Ref: strings.Repeat("x", 129), Text: &canary}, home),
		Packet(Request{Command: "click", Ref: "@stest:e1", Text: &canary}, home),
		Packet(Request{Command: "clear", Ref: "@stest:e1", App: canary}, home),
	} {
		var out bytes.Buffer
		runHelper(bytes.NewReader(packet), &out, []string{filepath.Join(home, canary)}, func(Request, string, string) ([]byte, error) {
			t.Error("invalid packet reached native loading")
			return nil, nil
		})
		if strings.TrimSpace(out.String()) != string(helperFailure) {
			t.Fatal("invalid packet diagnostics were not fixed")
		}
	}
	for n := 0; n < len(valid); n++ {
		if _, _, err := ReadPacket(bytes.NewReader(valid[:n])); err == nil {
			t.Fatal("truncated packet accepted")
		}
	}
}

func TestDesktopHelperInfrastructureFailure(t *testing.T) {
	home := t.TempDir()
	packet := Packet(Request{Command: "status"}, home)
	for _, run := range []func(Request, string, string) ([]byte, error){
		func(Request, string, string) ([]byte, error) { panic("private-panic-credential") },
		func(Request, string, string) ([]byte, error) { return nil, errors.New("private-loader-path") },
		func(Request, string, string) ([]byte, error) { return nil, nil },
		func(Request, string, string) ([]byte, error) {
			return bytes.Repeat([]byte{'x'}, maxNativeOutput+1), nil
		},
	} {
		var out bytes.Buffer
		runHelper(bytes.NewReader(packet), &out, []string{filepath.Join(home, "library")}, run)
		if strings.TrimSpace(out.String()) != string(helperFailure) {
			t.Fatal("infrastructure failure exposed diagnostics")
		}
	}
	for _, args := range [][]string{nil, {"relative"}, {home, "extra"}} {
		var out bytes.Buffer
		runHelper(bytes.NewReader(packet), &out, args, func(Request, string, string) ([]byte, error) {
			t.Error("invalid arguments reached native code")
			return nil, nil
		})
		if strings.TrimSpace(out.String()) != string(helperFailure) {
			t.Fatal("argument diagnostics changed")
		}
	}
	// Valid native failure envelopes remain byte-for-byte intact, including disposition.
	want := []byte(`{"ok":false,"error":{"code":"STALE_REF","disposition":{"delivery":"not_delivered","retry":"safe"}}}`)
	var out bytes.Buffer
	runHelper(bytes.NewReader(packet), &out, []string{filepath.Join(home, "library")}, func(r Request, root, library string) ([]byte, error) {
		if r.Command != "status" || root != home || library != filepath.Join(home, "library") {
			t.Error("helper dispatch arguments changed")
		}
		return want, nil
	})
	if !bytes.Equal(bytes.TrimSpace(out.Bytes()), want) {
		t.Fatal("native error envelope changed")
	}
	// Do not add a delimiter byte beyond the worker's capture ceiling.
	boundary := bytes.Repeat([]byte{'x'}, maxNativeOutput)
	out.Reset()
	runHelper(bytes.NewReader(packet), &out, []string{filepath.Join(home, "library")}, func(Request, string, string) ([]byte, error) { return boundary, nil })
	if !bytes.Equal(out.Bytes(), boundary) {
		t.Fatal("helper changed the output boundary")
	}
}
