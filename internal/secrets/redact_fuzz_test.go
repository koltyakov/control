package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

const redactionMarker = "[REDACTED]"

// claimedEncodings lists, independently of the redactor, the forms its
// documentation promises to mask: exact text, JSON string escaping, URL query
// and path escaping, and padded/unpadded standard and URL-safe base64.
func claimedEncodings(value string) []string {
	if value == "" {
		return nil
	}
	quoted, _ := json.Marshal(value)
	raw := []byte(value)
	return []string{
		value, string(quoted[1 : len(quoted)-1]), url.QueryEscape(value), url.PathEscape(value),
		base64.StdEncoding.EncodeToString(raw), base64.RawStdEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw), base64.RawURLEncoding.EncodeToString(raw),
	}
}

// checkMasked requires that no claimed encoding of any secret remains in text
// outside the markers the redactor inserted. The marker has no border (no
// proper prefix equals a suffix), so splitting cannot join text across it.
func checkMasked(t *testing.T, where, text string, values ...string) {
	t.Helper()
	for _, segment := range strings.Split(text, redactionMarker) {
		for _, value := range values {
			for _, encoded := range claimedEncodings(value) {
				if strings.Contains(segment, encoded) {
					t.Fatalf("%s: %q survives as %q in %q", where, value, encoded, text)
				}
			}
		}
	}
}

func redactionSeeds(f *testing.F) {
	for _, seed := range [][5]string{
		{"hunter2", "", "password=", "&user=x", ""},
		{"p@ss w/rd&", "tok-123", "Authorization: Bearer ", "\n", "q=p%40ss+w%2Frd%26"},
		{"a", "aa", "aaaa", "a", ""},
		{"ab", "bcdef", "xx", "cdef", "abcdef"},
		{"<&>", "\"quoted\"", `{"k":"`, `"}`, `<&>`},
		{"secret", "c2VjcmV0", "base64:", "c2VjcmV0", "c2VjcmV0"},
		{"\xff\xfe", "é", "invalid:", "", "��"},
		{"]x[", "D]", "[REDACTED]", "[REDACTED]x[", ""},
		{"\x00", "\t", "nul", "tab\tline", ""},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3], seed[4], uint8(0))
		f.Add(seed[0], seed[1], seed[2], seed[3], seed[4], uint8(5))
	}
}

// buildText embeds one claimed encoding of each secret between arbitrary
// surrounding text, so the fuzzer explores boundaries that overlap a secret.
func buildText(first, second, prefix, middle, suffix string, form uint8) string {
	pick := func(value string, n uint8) string {
		encodings := claimedEncodings(value)
		if len(encodings) == 0 {
			return ""
		}
		return encodings[int(n)%len(encodings)]
	}
	return prefix + pick(first, form) + middle + pick(second, form/8) + suffix
}

// FuzzRedactorText checks redaction completeness for arbitrary secrets,
// surrounding text and boundaries: no claimed encoding of a configured value
// remains outside an inserted marker, redaction is idempotent, and text
// without secrets is unchanged.
func FuzzRedactorText(f *testing.F) {
	redactionSeeds(f)
	f.Fuzz(func(t *testing.T, first, second, prefix, middle, suffix string, form uint8) {
		redactor := NewRedactor(map[string]string{"first": first, "second": second})
		text := buildText(first, second, prefix, middle, suffix, form)
		got := redactor.Text(text)
		checkMasked(t, "text", got, first, second)
		if (first == "" && second == "") != (redactor.replacer == nil) {
			t.Fatal("empty secrets must disable redaction, others must enable it")
		}
		if first == "" && second == "" && got != text {
			t.Fatalf("changed text without secrets: %q", got)
		}
		// Callers concatenate complete outputs before masking; masking the
		// concatenation of two outputs must still leave no secret.
		joined := redactor.Text(prefix + text + middle)
		checkMasked(t, "joined", joined, first, second)
	})
}

// FuzzRedactorJSON checks that JSON redaction keeps a valid document and masks
// every decoded string and object key, including keys merged after masking.
// Command output additionally masks the re-encoded text, as exec.run does.
func FuzzRedactorJSON(f *testing.F) {
	for _, seed := range []struct{ document, first, second string }{
		{`{"password":"hunter2","list":["hunter2",1,null,true]}`, "hunter2", ""},
		{`{"hunter2":"x","hunter3":"y"}`, "hunter2", "hunter3"},
		{`"hunter2"`, "hunter2", ""},
		{`[12345678, "12345678", {"n": 1.5e3}]`, "12345678", "1.5e3"},
		{`{"a":"<script>"}`, "<script>", ""},
		{`{"k":"aGVsbG8="}`, "hello", ""},
		{`["\ud800", "�"]`, "\xff", ""},
		{`[[[[[[[[[["deep"]]]]]]]]]]`, "deep", "ee"},
		{`{"a":1} trailing`, "a", ""},
	} {
		f.Add(seed.document, seed.first, seed.second)
	}
	f.Fuzz(func(t *testing.T, document, first, second string) {
		redactor := NewRedactor(map[string]string{"first": first, "second": second})
		clean, err := redactor.JSON([]byte(document))
		if err != nil {
			return
		}
		if !json.Valid(clean) {
			t.Fatalf("redaction produced invalid JSON %q", clean)
		}
		decoder := json.NewDecoder(bytes.NewReader(clean))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		var walk func(any)
		walk = func(v any) {
			switch v := v.(type) {
			case string:
				checkMasked(t, "string", v, first, second)
			case []any:
				for _, item := range v {
					walk(item)
				}
			case map[string]any:
				for key, item := range v {
					checkMasked(t, "key", key, first, second)
					walk(item)
				}
			}
		}
		walk(value)
		// exec.run applies text masking to the re-encoded document so numeric
		// tokens are also covered.
		checkMasked(t, "command output", redactor.Text(string(clean)), first, second)
	})
}
