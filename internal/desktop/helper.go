package desktop

import (
	"encoding/binary"
	"errors"
	"io"
	"path/filepath"
)

const maxNativeOutput = 2 << 20

var helperFailure = []byte(`{"ok":false,"error":{"code":"NATIVE_HELPER_FAILED","disposition":{"delivery":"unknown","retry":"unsafe"}}}`)

// ReadPacket validates the complete private-pipe request before native loading.
func ReadPacket(in io.Reader) (Request, string, error) {
	fail := errors.New("invalid native desktop packet")
	var magic [8]byte
	if _, err := io.ReadFull(in, magic[:]); err != nil || string(magic[:]) != "CDSK0001" {
		return Request{}, "", fail
	}
	fields := make([]string, 0, 5)
	for _, limit := range []uint32{16, 4096, 128, 4096, 32768} {
		var length uint32
		if binary.Read(in, binary.BigEndian, &length) != nil || length > limit {
			return Request{}, "", fail
		}
		b := make([]byte, length)
		if _, err := io.ReadFull(in, b); err != nil || !validText(string(b), int(limit)) {
			return Request{}, "", fail
		}
		fields = append(fields, string(b))
	}
	var depth uint32
	var tail [1]byte
	if binary.Read(in, binary.BigEndian, &depth) != nil || depth < 1 || depth > 32 {
		return Request{}, "", fail
	}
	if n, err := in.Read(tail[:]); n != 0 || err != io.EOF || !filepath.IsAbs(fields[4]) {
		return Request{}, "", fail
	}
	r := Request{Command: fields[0], App: fields[1], Ref: fields[2]}
	switch r.Command {
	case "status":
		if r.App != "" || r.Ref != "" || fields[3] != "" {
			return Request{}, "", fail
		}
	case "snapshot":
		if r.App == "" || r.Ref != "" || fields[3] != "" {
			return Request{}, "", fail
		}
		r.MaxDepth = int(depth)
	case "setValue", "click", "clear":
		if r.App != "" || !refPattern.MatchString(r.Ref) || (r.Command != "setValue" && fields[3] != "") {
			return Request{}, "", fail
		}
		if r.Command == "setValue" {
			r.Text = &fields[3]
		}
	default:
		return Request{}, "", fail
	}
	return r, fields[4], nil
}

// RunHelper is called only in Control's dedicated, one-request subprocess mode.
// It never reads credentials or configuration files, or prints diagnostics.
func RunHelper(in io.Reader, out io.Writer, args []string) {
	runHelper(in, out, args, callNative)
}

func runHelper(in io.Reader, out io.Writer, args []string, execute func(Request, string, string) ([]byte, error)) {
	response := helperFailure
	func() {
		// Do not expose panic values, native diagnostics, input, or paths.
		defer func() { _ = recover() }()
		if len(args) != 1 || !filepath.IsAbs(args[0]) {
			return
		}
		r, home, err := ReadPacket(in)
		if err != nil {
			return
		}
		if result, err := execute(r, home, args[0]); err == nil && len(result) > 0 && len(result) <= maxNativeOutput {
			response = result
		}
	}()
	_, _ = out.Write(response)
}
