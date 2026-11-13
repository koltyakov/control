package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestExecutableSHA256IdentifiesActualBytes(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	got, err := ExecutableSHA256()
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("actual executable digest: %q, %v", got, err)
	}
}
