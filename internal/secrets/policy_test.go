package secrets

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestHostPatternCanonicalForm(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"API.Example.COM":       "api.example.com",
		"api.example.com.":      "api.example.com",
		"*.Api.Example.com.":    "*.api.example.com",
		"api.example.com:0443":  "api.example.com:443",
		"192.0.2.10":            "192.0.2.10",
		"192.0.2.10:8443":       "192.0.2.10:8443",
		"[2001:DB8::1]":         "[2001:db8::1]",
		"[2001:db8:0::1]:8443":  "[2001:db8::1]:8443",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
		"localhost":             "localhost",
	} {
		p, err := ParseHostPattern(raw)
		if err != nil || p.String() != want {
			t.Errorf("ParseHostPattern(%q) = %q, %v; want %q", raw, p.String(), err, want)
		}
	}
	for _, raw := range []string{
		"", "*", "*.", "*.com", "**.example.com", "a.*.example.com", "*example.com", "api.example.com*",
		"api.example.com:", "api.example.com:0", "api.example.com:65536", "api.example.com:-1", "api.example.com:+443", "api.example.com:https",
		"2001:db8::1", "[2001:db8::1", "[2001:db8::1]x", "[fe80::1%en0]", "[::ffff:192.0.2.1]", "[192.0.2.1]", "*.192.0.2.1",
		"bücher.example", "api..example.com", "api.example.com..", ".api.example.com", "api_internal.example.com",
		"127.1", "0x7f.1", "example.123", "user@api.example.com", "api.example.com/path", "https://api.example.com",
		strings.Repeat("a", 64) + ".example.com", strings.Repeat("a.", 130) + "com",
	} {
		if p, err := ParseHostPattern(raw); err == nil {
			t.Errorf("ParseHostPattern(%q) accepted %q", raw, p.String())
		}
	}
}

func mustPolicy(t *testing.T, uses []string, hosts []string, plain bool) *Policy {
	t.Helper()
	p, err := NewPolicy(uses, hosts, plain)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPolicyPermitsDestination(t *testing.T) {
	t.Parallel()
	p := mustPolicy(t, []string{"http"}, []string{"api.example.com", "*.api.example.com", "files.example.com:8443", "192.0.2.10", "[2001:db8::1]:9443"}, false)
	plain := mustPolicy(t, []string{"http"}, []string{"intranet.example.com"}, true)
	for _, tc := range []struct {
		policy *Policy
		url    string
		want   bool
	}{
		{p, "https://api.example.com/v1", true},
		{p, "https://API.EXAMPLE.COM/v1", true},
		{p, "https://api.example.com./v1", true},
		{p, "https://api.example.com:443/v1", true},
		{p, "https://api.example.com:0443/v1", true},
		{p, "https://api.example.com:/v1", true},
		{p, "https://eu.api.example.com/v1", true},
		{p, "https://a.b.api.example.com/v1", true},
		{p, "https://files.example.com:8443/x", true},
		{p, "https://192.0.2.10/x", true},
		{p, "https://[2001:db8::1]:9443/x", true},
		{p, "https://api%2eexample.com/v1", false}, // url.Parse refuses ASCII escapes in hosts.
		{p, "https://b%C3%BCcher.api.example.com/x", false},

		{p, "http://api.example.com/v1", false},       // https only by default
		{p, "https://api.example.com:8443/v1", false}, // no port means the default port only
		{p, "https://files.example.com/x", false},     // explicit port only
		{p, "https://api.example.com.evil.test/v1", false},
		{p, "https://evilapi.example.com/v1", false},
		{p, "https://example.com/v1", false},
		{p, "https://api.example.co/v1", false},
		{p, "https://api.example.com..//v1", false},
		{p, "https://user@api.example.com/v1", false},
		{p, "https://user:pass@api.example.com/v1", false},
		{p, "https://api.example.com@evil.test/v1", false},
		{p, "https://192.0.2.11/x", false},
		{p, "https://[::ffff:192.0.2.10]/x", false},
		{p, "https://3221225994/x", false}, // 192.0.2.10 as an integer
		{p, "https://192.0.2.010/x", false},
		{p, "https://[2001:db8::1]/x", false},
		{p, "https://[fe80::1%25en0]/x", false},
		{p, "https://bücher.api.example.com/x", false},
		{p, "https://xn--bcher-kva.api.example.com/x", true},
		{p, "ftp://api.example.com/x", false},
		{p, "https:api.example.com", false},

		{plain, "http://intranet.example.com/", true},
		{plain, "https://intranet.example.com/", true},
		{plain, "http://intranet.example.com:443/", false},
		{plain, "http://intranet.example.com:80/", true},
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			if tc.want {
				t.Errorf("parse %s: %v", tc.url, err)
			}
			continue
		}
		if got := tc.policy.permits(UseHTTP, u); got != tc.want {
			t.Errorf("permits(%s) = %v, want %v", tc.url, got, tc.want)
		}
	}
	if p.permits(UseHTTP, nil) || p.permits(UseExec, nil) || p.permits(UseDesktop, nil) {
		t.Fatal("http-only policy permitted another use or no destination")
	}
	all := mustPolicy(t, []string{"exec", "desktop"}, nil, false)
	if !all.permits(UseExec, nil) || !all.permits(UseDesktop, nil) || all.permits(UseHTTP, &url.URL{Scheme: "https", Host: "api.example.com"}) {
		t.Fatal("use allow-list ignored")
	}
}

func TestPolicyValidation(t *testing.T) {
	t.Parallel()
	p := mustPolicy(t, []string{"http", " exec", "http"}, []string{"B.example.com", "a.example.com", "b.example.com."}, true)
	if got := string(must(json.Marshal(p))); got != `{"uses":["exec","http"],"hosts":["a.example.com","b.example.com"],"plainHTTP":true}` {
		t.Fatalf("canonical policy %s", got)
	}
	for _, bad := range []func() (*Policy, error){
		func() (*Policy, error) { return NewPolicy(nil, nil, false) },
		func() (*Policy, error) { return NewPolicy([]string{"ssh"}, nil, false) },
		func() (*Policy, error) { return NewPolicy([]string{""}, nil, false) },
		func() (*Policy, error) { return NewPolicy([]string{"http"}, nil, false) },
		func() (*Policy, error) { return NewPolicy([]string{"exec"}, []string{"api.example.com"}, false) },
		func() (*Policy, error) { return NewPolicy([]string{"exec"}, nil, true) },
		func() (*Policy, error) {
			hosts := make([]string, MaxPolicyHosts+1)
			for i := range hosts {
				hosts[i] = strings.Repeat("a", i+1) + ".example.com"
			}
			return NewPolicy([]string{"http"}, hosts, false)
		},
		func() (*Policy, error) {
			hosts := make([]string, MaxPolicyHosts)
			for i := range hosts {
				hosts[i] = strings.Repeat(string(rune('a'+i)), 63) + "." + strings.Repeat("b", 63) + ".example.com"
			}
			return NewPolicy([]string{"http"}, hosts, false)
		},
	} {
		if p, err := bad(); err == nil {
			t.Errorf("accepted invalid policy %+v", p)
		}
	}
	for _, raw := range []string{
		`null`, `{}`, `[]`, `"exec"`, `{"uses":[]}`, `{"uses":["exec"],"extra":true}`, `{"uses":["exec"]} {}`,
		`{"uses":["http","exec"]}`, `{"uses":["exec","exec"]}`, `{"uses":["http"],"hosts":["API.example.com"]}`,
		`{"uses":["http"],"hosts":["b.example.com","a.example.com"]}`, `{"uses":["http"],"hosts":["a.example.com","a.example.com"]}`,
		`{"uses":["http"]}`, `{"uses":["Exec"]}`, `{"uses":["exec"],"plainHTTP":true}`,
	} {
		if p, err := DecodePolicy([]byte(raw)); err == nil {
			t.Errorf("DecodePolicy(%s) accepted %+v", raw, p)
		}
	}
	if _, err := DecodePolicy([]byte(`{"uses":["exec"],"hosts":[]}`)); err != nil {
		t.Fatalf("empty host list is canonical for non-http policies: %v", err)
	}
	if _, err := DecodePolicy(append([]byte(`{"uses":["exec"]}`), make([]byte, MaxPolicy)...)); err == nil {
		t.Fatal("accepted oversized policy")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
