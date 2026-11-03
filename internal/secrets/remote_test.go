package secrets

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSecretEnvelope(t *testing.T) {
	t.Parallel()
	key, err := NewManagementKey()
	if err != nil {
		t.Fatal(err)
	}
	const value = "  private-\"credential\n世界  "
	e, err := Seal("worker", "app.password", value, key.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(e)
	if bytes.Contains(data, []byte("private-")) {
		t.Fatal("envelope contains plaintext")
	}
	if got, err := Open(key, "worker", "app.password", e); err != nil || got != value {
		t.Fatalf("round trip failed: %v", err)
	}
	for _, mutate := range []func(*Envelope){
		func(e *Envelope) { e.ID = strings.Repeat("0", 32) },
		func(e *Envelope) { e.ExpiresAt++ },
		func(e *Envelope) { e.Nonce[0] ^= 1 },
		func(e *Envelope) { e.Ciphertext[0] ^= 1 },
		func(e *Envelope) { e.ExpiresAt = time.Now().Unix() },
		func(e *Envelope) { e.ExpiresAt = time.Now().Add(2 * time.Minute).Unix() },
		func(e *Envelope) { e.Key = []byte("invalid") },
	} {
		var changed Envelope
		_ = json.Unmarshal(data, &changed)
		mutate(&changed)
		if _, err := Open(key, "worker", "app.password", changed); err == nil || strings.Contains(err.Error(), "private-") {
			t.Fatal("tampered envelope accepted or credential leaked")
		}
	}
	other, _ := NewManagementKey()
	if _, err := Open(other, "worker", "app.password", e); err == nil {
		t.Fatal("another process decrypted envelope")
	}
	for _, pair := range [][2]string{{"another-worker", "app.password"}, {"worker", "another.name"}} {
		if _, err := Open(key, pair[0], pair[1], e); err == nil {
			t.Fatal("envelope binding ignored")
		}
	}
	for _, bad := range []string{"", "nul\x00", strings.Repeat("x", MaxValue+1), string([]byte{255})} {
		if _, err := Seal("worker", "api", bad, key.PublicKey().Bytes()); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
}

func TestSecretManagementSecureURL(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"https://control.example.com", "http://localhost:7330", "http://127.0.0.1:7330", "http://[::1]:7330"} {
		if !SecureURL(value) {
			t.Fatalf("rejected secure URL %s", value)
		}
	}
	for _, value := range []string{"", "http://control.example.com", "http://127.0.0.1.example.com", "https://user:pass@control.example.com", "https://control.example.com?query", "ws://localhost:7330"} {
		if SecureURL(value) {
			t.Fatalf("accepted insecure URL %s", value)
		}
	}
}
