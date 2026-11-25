package secrets

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPolicyEnvelope(t *testing.T) {
	t.Parallel()
	key, err := NewManagementKey()
	if err != nil {
		t.Fatal(err)
	}
	public := key.PublicKey().Bytes()
	policy := mustPolicy(t, []string{"http"}, []string{"api.example.com"}, false)
	e, err := SealPolicy("worker", "api.token", "private-value", policy, public)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(e)
	if e.Version != PolicyVersion || bytes.Contains(data, []byte("private-")) || bytes.Contains(data, []byte("api.example.com")) {
		t.Fatalf("policy envelope is not versioned or exposes plaintext: %s", data)
	}
	w, err := OpenWrite(key, "worker", "api.token", e)
	if err != nil || w.Value != "private-value" || !w.ReplacePolicy || !reflect.DeepEqual(w.Policy, policy) {
		t.Fatalf("round trip: %+v %v", w, err)
	}
	if _, err := Open(key, "worker", "api.token", e); err == nil {
		t.Fatal("value-only Open accepted a policy-bearing envelope")
	}
	// An explicit clear is distinct from a value-only write.
	cleared, err := SealPolicy("worker", "api.token", "private-value", nil, public)
	if err != nil {
		t.Fatal(err)
	}
	if w, err := OpenWrite(key, "worker", "api.token", cleared); err != nil || !w.ReplacePolicy || w.Policy != nil {
		t.Fatalf("explicit clear: %+v %v", w, err)
	}
	plain, err := Seal("worker", "api.token", "private-value", public)
	if err != nil {
		t.Fatal(err)
	}
	if w, err := OpenWrite(key, "worker", "api.token", plain); err != nil || w.ReplacePolicy || w.Value != "private-value" {
		t.Fatalf("value-only write: %+v %v", w, err)
	}
	// The version selects the key derivation and AAD, so a relay cannot
	// strip a policy by relabelling the envelope, or add one.
	for _, mutate := range []func(*Envelope){
		func(e *Envelope) { e.Version = 0 },
		func(e *Envelope) { e.Version = 3 },
		func(e *Envelope) { e.Ciphertext[len(e.Ciphertext)/2] ^= 1 },
		func(e *Envelope) { e.Ciphertext = e.Ciphertext[:len(e.Ciphertext)-1] },
		func(e *Envelope) { e.ExpiresAt++ },
	} {
		var changed Envelope
		_ = json.Unmarshal(data, &changed)
		mutate(&changed)
		if w, err := OpenWrite(key, "worker", "api.token", changed); err == nil {
			t.Fatalf("tampered policy envelope opened: %+v", w)
		}
	}
	upgraded := plain
	upgraded.Version = PolicyVersion
	if _, err := OpenWrite(key, "worker", "api.token", upgraded); err == nil {
		t.Fatal("relabelled value-only envelope opened as a policy write")
	}
	if _, err := SealPolicy("worker", "api.token", "v", &Policy{Uses: []Use{UseHTTP}}, public); err == nil {
		t.Fatal("sealed an invalid policy")
	}
	// Malformed plaintexts sealed under the policy protocol are refused.
	for _, plaintext := range []string{"no separator", "\x00value", `{"uses":["exec"],"x":1}` + "\x00v", `{"uses":["http"],"hosts":["B.example.com"]}` + "\x00v", `null` + "\x00", `{"uses":["exec"]}` + "\x00" + "a\x00b"} {
		e, err := seal("worker", "api.token", []byte(plaintext), PolicyVersion, public)
		if err != nil {
			t.Fatal(err)
		}
		if w, err := OpenWrite(key, "worker", "api.token", e); err == nil {
			t.Fatalf("opened malformed plaintext %q: %+v", plaintext, w)
		}
	}
	// A maximal policy and value fit the remote request bound.
	hosts := make([]string, 0, MaxPolicyHosts)
	for i := range MaxPolicyHosts {
		hosts = append(hosts, strings.Repeat(string(rune('a'+i)), 40)+".example.com")
	}
	large := mustPolicy(t, []string{"desktop", "exec", "http"}, hosts, true)
	big, err := SealPolicy("worker", strings.Repeat("n", 128), strings.Repeat("\x01", MaxValue), large, public)
	if err != nil {
		t.Fatal(err)
	}
	q := ManagementRequest{ID: big.ID, Operation: "set", Name: strings.Repeat("n", 128), ExpiresAt: big.ExpiresAt, Envelope: &big}
	if !q.Valid(time.Now()) || len(must(json.Marshal(q))) > MaxRemoteMessage {
		t.Fatal("maximal policy envelope exceeds the request bounds")
	}
	q.Policies = true
	if q.Valid(time.Now()) {
		t.Fatal("set request accepted a listing flag")
	}
}
