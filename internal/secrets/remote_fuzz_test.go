package secrets

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// FuzzOpenEnvelope decodes arbitrary JSON as a management request and, for a
// genuinely sealed envelope, applies one fuzzer-chosen change to its
// ciphertext, nonce, key, ID, expiry, recipient node, name or version. A
// field of 128 or more seals a policy-bearing envelope instead of a
// value-only one. Only the unmodified envelope may open, and it must return
// the sealed value and, for policy envelopes, the sealed policy.
func FuzzOpenEnvelope(f *testing.F) {
	private, err := NewManagementKey()
	if err != nil {
		f.Fatal(err)
	}
	public := private.PublicKey().Bytes()
	for _, seed := range []struct {
		raw    string
		value  string
		field  uint8
		offset uint16
		flip   byte
	}{
		{`{}`, "hunter2", 0, 0, 0},
		{`{"id":"00112233445566778899aabbccddeeff","operation":"set","name":"db.password","expiresAt":1,"envelope":{"key":"AAAA","nonce":"","ciphertext":""}}`, "v", 1, 0, 1},
		{`{"id":"x","operation":"list","expiresAt":9223372036854775807}`, "value with spaces", 2, 3, 0x80},
		{`{"envelope":{"key":null,"nonce":"AAAAAAAAAAAAAAAA","ciphertext":"` + string(bytes.Repeat([]byte("A"), 40)) + `"}}`, "é", 3, 64, 0xff},
		{`not json`, "x", 4, 0, 1},
		{`{"operation":"delete","name":"a"}`, "y", 5, 0, 1},
		{`[]`, "z", 6, 0, 1},
		{`{"envelope":{"id":""}}`, "w", 7, 0, 1},
		{`{"envelope":{"version":2}}`, "policy-value", 128, 0, 0},
		{`{"operation":"list","policies":true}`, "p", 128, 5, 1},
		{`{}`, "p", 131, 9, 0x40},
		{`{}`, "p", 136, 0, 2},
		{`{}`, "p", 8, 0, 2},
	} {
		f.Add([]byte(seed.raw), seed.value, seed.field, seed.offset, seed.flip)
	}
	f.Fuzz(func(t *testing.T, raw []byte, value string, field uint8, offset uint16, flip byte) {
		now := time.Now()
		var request ManagementRequest
		if json.Unmarshal(raw, &request) == nil {
			if request.Valid(now) {
				// Only "set" carries an envelope, and then one bound to the request.
				switch {
				case request.Operation == "set" && request.Envelope != nil && request.Envelope.ID == request.ID && request.Envelope.Valid(now):
				case (request.Operation == "list" || request.Operation == "delete") && request.Envelope == nil:
				default:
					t.Fatalf("accepted inconsistent request %+v", request)
				}
			}
			// A fuzzer cannot forge GCM authentication under a fresh key.
			if request.Envelope != nil {
				if got, err := OpenWrite(private, "node", request.Name, *request.Envelope); err == nil {
					t.Fatalf("opened an unsealed envelope: %+v", got)
				}
			}
		}
		withPolicy := field >= 128
		field &= 0x7f
		policy := &Policy{Uses: []Use{UseHTTP}, Hosts: []string{"*.example.com", "api.example.net:8443"}}
		var envelope Envelope
		var err error
		if withPolicy {
			envelope, err = SealPolicy("node", "db.password", value, policy, public)
		} else {
			envelope, err = Seal("node", "db.password", value, public)
		}
		if err != nil {
			if validValue(value) {
				t.Fatalf("cannot seal a valid value: %v", err)
			}
			return
		}
		node, name := "node", "db.password"
		changed := flip != 0
		mutate := func(b []byte) []byte {
			if len(b) == 0 || !changed {
				return b
			}
			b = bytes.Clone(b)
			b[int(offset)%len(b)] ^= flip
			return b
		}
		switch field % 9 {
		case 0:
			envelope.Ciphertext = mutate(envelope.Ciphertext)
		case 1:
			envelope.Nonce = mutate(envelope.Nonce)
		case 2:
			envelope.Key = mutate(envelope.Key)
		case 3:
			envelope.ID = string(mutate([]byte(envelope.ID)))
		case 4:
			if changed {
				envelope.ExpiresAt -= int64(flip%8) + 1
			}
		case 5:
			node = string(mutate([]byte(node)))
		case 6:
			name = string(mutate([]byte(name)))
		case 7:
			if changed {
				envelope.Ciphertext = envelope.Ciphertext[:int(offset)%len(envelope.Ciphertext)]
			}
		case 8:
			// Relabelling must not strip a policy or reinterpret a value.
			envelope.Version ^= int(flip)
		}
		got, err := OpenWrite(private, node, name, envelope)
		if !changed {
			if err != nil || got.Value != value || got.ReplacePolicy != withPolicy || (withPolicy && !reflect.DeepEqual(got.Policy, policy)) {
				t.Fatalf("unmodified envelope: %+v, %v", got, err)
			}
			if plain, err := Open(private, node, name, envelope); withPolicy == (err == nil) || (!withPolicy && plain != value) {
				t.Fatalf("value-only Open of policy=%v envelope: %q, %v", withPolicy, plain, err)
			}
			return
		}
		if err == nil {
			t.Fatalf("field %d changed by %#x at %d still opened: %+v", field%9, flip, offset, got)
		}
	})
}
