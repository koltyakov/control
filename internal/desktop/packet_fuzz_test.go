package desktop

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/koltyakov/control/internal/secrets"
)

func packetBytes(depth uint32, fields ...string) []byte {
	b := []byte("CDSK0001")
	for _, field := range fields {
		b = binary.BigEndian.AppendUint32(b, uint32(len(field)))
		b = append(b, field...)
	}
	return binary.BigEndian.AppendUint32(b, depth)
}

// FuzzDesktopPacket checks the private helper pipe in both directions. Any
// native request Decode accepts, after worker-side secret resolution, must
// survive Packet and ReadPacket unchanged with its state directory, so the
// two field limits cannot disagree. Arbitrary pipe bytes accepted by
// ReadPacket must describe a request that re-encodes to an equal packet.
func FuzzDesktopPacket(f *testing.F) {
	home := filepath.Join(string(filepath.Separator)+"private", "state")
	if !filepath.IsAbs(home) {
		home = `C:\private\state`
	}
	for _, seed := range []struct {
		args, home, secret string
		raw                []byte
	}{
		{`{"command":"status"}`, home, "", packetBytes(10, "status", "", "", "", home)},
		{`{"command":"snapshot","app":"Finder","maxDepth":32}`, home, "", packetBytes(32, "snapshot", "Finder", "", "", home)},
		{`{"command":"snapshot","app":"Finder"}`, home, "", packetBytes(33, "snapshot", "Finder", "", "", home)},
		{`{"command":"click","ref":"@s123:e1"}`, home, "", packetBytes(1, "click", "", "@s123:e1", "", home)},
		{`{"command":"setValue","ref":"@s123:e1","text":"pipe-only"}`, home, "", packetBytes(10, "setValue", "", "@s123:e1", "pipe-only", home)},
		{`{"command":"setValue","ref":"@s123:e1","secret":"app.password"}`, home, "hunter2", packetBytes(10, "setValue", "", "@s123:e1", "x\x00", home)},
		{`{"command":"clear","ref":"@s123:e9999999999"}`, "relative", "", packetBytes(10, "clear", "", "@s123:e1", "", "relative")},
		{`{"command":"cli","args":["version"]}`, home, "", packetBytes(0, "status", "", "", "", home)},
		{`{"command":"status","extra":1}`, home, "", append(packetBytes(10, "status", "", "", "", home), 0)},
		{`{"command":"snapshot","app":"\u0000"}`, home, "", []byte("CDSK0001\xff\xff\xff\xff")},
	} {
		f.Add(seed.args, seed.home, seed.secret, seed.raw)
	}
	f.Fuzz(func(t *testing.T, args, home, secret string, raw []byte) {
		if request, err := Decode([]byte(args)); err == nil && request.Command != "cli" {
			if request.Secret != "" {
				// The worker substitutes a stored value, itself bounded by the
				// secret store, before building the packet.
				if secret == "" || len(secret) > secrets.MaxValue || !validText(secret, secrets.MaxValue) {
					secret = "stored-value"
				}
				request.Text, request.Secret = &secret, ""
			}
			got, gotHome, err := ReadPacket(bytes.NewReader(Packet(request, home)))
			wantHome := filepath.IsAbs(home) && validText(home, 32768)
			if (err == nil) != wantHome {
				t.Fatalf("home %q accepted %v: %v", home, wantHome, err)
			}
			if err == nil {
				want := request
				if want.Command == "snapshot" && want.MaxDepth == 0 {
					want.MaxDepth = 10
				}
				if gotHome != home || !reflect.DeepEqual(got, want) {
					t.Fatalf("packet changed %+v to %+v (home %q)", want, got, gotHome)
				}
			}
		}
		request, gotHome, err := ReadPacket(bytes.NewReader(raw))
		if err != nil {
			return
		}
		encoded := Packet(request, gotHome)
		again, againHome, err := ReadPacket(bytes.NewReader(encoded))
		if err != nil || againHome != gotHome || !reflect.DeepEqual(again, request) {
			t.Fatalf("accepted packet does not re-encode: %+v, %v", again, err)
		}
		if request.Command == "snapshot" && !bytes.Equal(encoded, raw) {
			t.Fatalf("snapshot packet is not canonical: %x != %x", encoded, raw)
		}
	})
}
