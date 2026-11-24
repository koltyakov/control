package upload

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"path"
	"strings"
	"testing"
)

func rawRecord(kind byte, size uint32, payload string) []byte {
	b := append([]byte{kind}, binary.BigEndian.AppendUint32(nil, size)...)
	return append(b, payload...)
}

// FuzzReadRecord checks the five-byte upload record header against arbitrary
// sender bytes. Accepted records stay within their kind's limit and the
// caller's scratch buffer and re-encode to the same bytes. Rejected sizes never
// read the payload.
func FuzzReadRecord(f *testing.F) {
	start := `{"generation":"g","offset":0,"prefixSHA256":"` + strings.Repeat("0", 64) + `"}`
	for _, seed := range []struct {
		data    []byte
		scratch uint32
	}{
		{rawRecord(StartRecord, uint32(len(start)), start), BlockSize + 8},
		{rawRecord(DataRecord, 12, "\x00\x00\x00\x00\x00\x00\x00\x00data"), BlockSize + 8},
		{rawRecord(FinishRecord, 0, ""), BlockSize + 8},
		{rawRecord(DataRecord, BlockSize+8, ""), BlockSize + 8},
		{rawRecord(DataRecord, BlockSize+9, ""), ^uint32(0)},
		{rawRecord(StartRecord, MetadataLimit+1, ""), ^uint32(0)},
		{rawRecord(DataRecord, 16, "short"), 8},
		{rawRecord(0, 0, ""), 64},
		{rawRecord(4, 0, ""), 64},
		{rawRecord(FinishRecord, 4, "ab"), 64},
		{[]byte{DataRecord, 0, 0}, 64},
	} {
		f.Add(seed.data, seed.scratch)
	}
	f.Fuzz(func(t *testing.T, data []byte, scratchSize uint32) {
		scratch := make([]byte, min(scratchSize, BlockSize+16))
		reader := bytes.NewReader(data)
		kind, payload, err := ReadRecord(reader, scratch)
		consumed := len(data) - reader.Len()
		if len(data) < 5 {
			if err == nil {
				t.Fatal("accepted a truncated header")
			}
			return
		}
		wantKind, size := data[0], binary.BigEndian.Uint32(data[1:5])
		limit := uint32(MetadataLimit)
		if wantKind == DataRecord {
			limit = BlockSize + 8
		}
		if wantKind < StartRecord || wantKind > FinishRecord || size > limit || uint64(size) > uint64(len(scratch)) {
			if err == nil || consumed != 5 {
				t.Fatalf("invalid kind %d or size %d (scratch %d) accepted or read: %v, consumed %d", wantKind, size, len(scratch), err, consumed)
			}
			return
		}
		if uint64(size) > uint64(len(data)-5) {
			if err == nil {
				t.Fatal("accepted a truncated payload")
			}
			return
		}
		if err != nil || kind != wantKind || consumed != 5+int(size) || !bytes.Equal(payload, data[5:5+size]) {
			t.Fatalf("record kind %d size %d: got kind %d, %d bytes, consumed %d: %v", wantKind, size, kind, len(payload), consumed, err)
		}
		if len(payload) != 0 && &payload[0] != &scratch[0] {
			t.Fatal("payload does not use caller-owned scratch storage")
		}
		var out bytes.Buffer
		if err := WriteRecord(&out, kind, payload); err != nil {
			t.Fatalf("cannot re-encode an accepted record: %v", err)
		}
		if !bytes.Equal(out.Bytes(), data[:consumed]) {
			t.Fatalf("re-encoded record differs: %x != %x", out.Bytes(), data[:consumed])
		}
	})
}

// windowsReservedName mirrors the Windows device-name rules applied by Go's
// filepath.IsLocal: the base ends at the first dot or colon, trailing spaces
// are ignored, and the comparison is case-insensitive.
func windowsReservedName(name string) bool {
	base := name
	if i := strings.IndexAny(base, ".:"); i >= 0 {
		base = base[:i]
	}
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	chars := []rune(base)
	return len(chars) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && strings.ContainsRune("123456789¹²³", chars[3])
}

// FuzzSpecValidate checks that accepted upload destinations are portable on
// every supported OS: normalized, relative, outside the staging directory,
// and free of separators, drive syntax, control characters and Windows device
// names. Accepted specifications survive a JSON round trip unchanged.
func FuzzSpecValidate(f *testing.F) {
	digest := strings.Repeat("a", 64)
	for _, seed := range []string{
		"out/file.bin", "a", "a/b/c.txt", "./a", "a/../b", "/abs", "a//b", "a/", "C:/x", `a\b`,
		".control-uploads/x", ".CONTROL-UPLOADS", "con", "CON.txt", "con .txt", "COM1", "com1 .log", "LPT¹", "COM0",
		"CONIN$", "nul:", "trailing.", "space ", "tab\tname", "é/ü", "\xff", strings.Repeat("x", 256),
	} {
		f.Add("upload-1", seed, int64(1), digest)
	}
	f.Add("", "a", int64(0), digest)
	f.Add("x", "a", int64(-1), digest)
	f.Add("x", "a", MaxSize+1, digest)
	f.Add("x", "a", int64(0), strings.ToUpper(digest))
	f.Fuzz(func(t *testing.T, id, destination string, size int64, sha string) {
		spec := Spec{ID: id, Path: destination, Size: size, SHA256: sha}
		if spec.Validate() != nil {
			return
		}
		if !ValidID(id) || size < 0 || size > MaxSize || len(sha) != 64 || strings.Trim(sha, "0123456789abcdef") != "" {
			t.Fatalf("accepted invalid metadata %+v", spec)
		}
		if path.IsAbs(destination) || path.Clean(destination) != destination || strings.ContainsAny(destination, `\:`) {
			t.Fatalf("accepted non-portable path %q", destination)
		}
		for _, part := range strings.Split(destination, "/") {
			if part == "" || part == "." || part == ".." || strings.EqualFold(part, StagingDir) {
				t.Fatalf("accepted reserved component %q in %q", part, destination)
			}
			if windowsReservedName(part) {
				t.Fatalf("accepted Windows device name %q in %q", part, destination)
			}
			if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.ContainsFunc(part, func(r rune) bool { return r < 32 || r == 127 }) {
				t.Fatalf("accepted a component Windows would rewrite or reject: %q", part)
			}
		}
		var decoded Spec
		if err := json.Unmarshal([]byte(mustJSON(t, spec)), &decoded); err != nil || decoded != spec || decoded.Validate() != nil {
			t.Fatalf("JSON round trip changed %+v to %+v: %v", spec, decoded, err)
		}
	})
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
