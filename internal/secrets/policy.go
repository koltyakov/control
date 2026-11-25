package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Use names a consumer that resolves secret references on a worker.
type Use string

const (
	// UseHTTP covers http.request and http.download headerSecrets.
	UseHTTP Use = "http"
	// UseExec covers exec.run envSecrets and stdinSecret.
	UseExec Use = "exec"
	// UseDesktop covers native desktop.run setValue.
	UseDesktop Use = "desktop"
)

const (
	// MaxPolicy bounds one encoded policy in storage and in remote envelopes.
	MaxPolicy = 2048
	// MaxPolicyHosts bounds the destination patterns of one policy.
	MaxPolicyHosts = 16
)

// Policy restricts which consumers may resolve one secret and, for HTTP, where
// it may be sent. A secret without a policy is unrestricted. A secret with a
// policy is refused for every use the policy does not allow.
//
// Policies are enforced by the worker when a reference resolves. They stop
// accidental and prompt-injected misuse through structured secret references,
// not a hostile process that can read the worker's files.
type Policy struct {
	// Uses lists the permitted consumers in canonical order: desktop, exec, http.
	Uses []Use `json:"uses"`
	// Hosts lists permitted HTTP destinations, required exactly when Uses
	// contains http. See ParseHostPattern for the grammar.
	Hosts []string `json:"hosts,omitempty"`
	// PlainHTTP additionally permits unencrypted http:// destinations.
	PlainHTTP bool `json:"plainHTTP,omitempty"`
}

var useOrder = []Use{UseDesktop, UseExec, UseHTTP}

// NewPolicy validates and canonicalizes a policy: uses are deduplicated and
// ordered, host patterns are normalized, deduplicated and sorted.
func NewPolicy(uses []string, hosts []string, plainHTTP bool) (*Policy, error) {
	if len(uses) == 0 {
		return nil, errors.New("a secret use policy requires at least one use: http, exec or desktop")
	}
	allowed := map[Use]bool{}
	for _, use := range uses {
		u := Use(strings.TrimSpace(use))
		if !slices.Contains(useOrder, u) {
			return nil, fmt.Errorf("unknown secret use %q; use http, exec or desktop", use)
		}
		allowed[u] = true
	}
	p := &Policy{PlainHTTP: plainHTTP}
	for _, u := range useOrder {
		if allowed[u] {
			p.Uses = append(p.Uses, u)
		}
	}
	if !allowed[UseHTTP] {
		if len(hosts) != 0 || plainHTTP {
			return nil, errors.New("host patterns and plain HTTP apply only to a policy that allows http")
		}
		return p, nil
	}
	if len(hosts) == 0 {
		return nil, errors.New("a policy that allows http requires at least one host pattern")
	}
	if len(hosts) > MaxPolicyHosts {
		return nil, fmt.Errorf("a secret use policy allows at most %d host patterns", MaxPolicyHosts)
	}
	seen := map[string]bool{}
	for _, raw := range hosts {
		pattern, err := ParseHostPattern(raw)
		if err != nil {
			return nil, err
		}
		if !seen[pattern.String()] {
			seen[pattern.String()] = true
			p.Hosts = append(p.Hosts, pattern.String())
		}
	}
	slices.Sort(p.Hosts)
	if err := p.checkSize(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Policy) checkSize() error {
	b, err := json.Marshal(p)
	if err != nil || len(b) > MaxPolicy {
		return fmt.Errorf("a secret use policy must encode to at most %d bytes", MaxPolicy)
	}
	return nil
}

// Validate reports whether p is already in the canonical form NewPolicy
// produces. Stored and received policies must be canonical, so a worker
// enforces exactly what was written.
func (p *Policy) Validate() error {
	if p == nil {
		return errors.New("missing secret use policy")
	}
	uses := make([]string, 0, len(p.Uses))
	for _, u := range p.Uses {
		uses = append(uses, string(u))
	}
	canonical, err := NewPolicy(uses, p.Hosts, p.PlainHTTP)
	if err != nil {
		return err
	}
	if !slices.Equal(canonical.Uses, p.Uses) || !slices.Equal(canonical.Hosts, p.Hosts) {
		return errors.New("secret use policy is not in canonical form")
	}
	return nil
}

// Clone returns an independent copy.
func (p *Policy) Clone() *Policy {
	if p == nil {
		return nil
	}
	return &Policy{Uses: slices.Clone(p.Uses), Hosts: slices.Clone(p.Hosts), PlainHTTP: p.PlainHTTP}
}

// DecodePolicy strictly decodes and validates one canonical encoded policy.
func DecodePolicy(raw []byte) (*Policy, error) {
	if len(raw) == 0 || len(raw) > MaxPolicy {
		return nil, errors.New("invalid secret use policy size")
	}
	var p Policy
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid secret use policy encoding")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// HostPattern is one parsed destination pattern.
//
// Grammar: an exact host name ("api.example.com"), a leading wildcard
// ("*.example.com"), an IPv4 literal ("192.0.2.10") or a bracketed IPv6
// literal ("[2001:db8::1]"), each optionally followed by ":PORT". Host names
// are ASCII, internationalized names in their "xn--" form, compared in
// lowercase with one trailing dot removed. A wildcard matches one or more labels in front of
// its suffix and never the suffix itself. IP literals match only the same
// literal address. Without a port a pattern matches only the default port of
// the request scheme (443, or 80 for permitted plain HTTP).
type HostPattern struct {
	host     string
	wildcard bool
	ip       netip.Addr
	port     int
}

// ParseHostPattern parses and normalizes one destination pattern.
func ParseHostPattern(raw string) (HostPattern, error) {
	var p HostPattern
	invalid := func(reason string) (HostPattern, error) {
		return HostPattern{}, fmt.Errorf("invalid host pattern %q: %s", raw, reason)
	}
	if raw == "" || len(raw) > 300 {
		return invalid("expected 1..300 bytes")
	}
	host, port := raw, ""
	if strings.HasPrefix(raw, "[") {
		end := strings.IndexByte(raw, ']')
		if end < 0 {
			return invalid("unterminated IPv6 literal")
		}
		host, port = raw[1:end], raw[end+1:]
		if port != "" {
			if !strings.HasPrefix(port, ":") {
				return invalid("expected :PORT after the IPv6 literal")
			}
			port = port[1:]
			if port == "" {
				return invalid("empty port")
			}
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" || addr.Is4In6() {
			return invalid("expected a bracketed IPv6 literal without a zone")
		}
		p.ip = addr
	} else {
		switch strings.Count(raw, ":") {
		case 0:
		case 1:
			host, port, _ = strings.Cut(raw, ":")
			if port == "" {
				return invalid("empty port")
			}
		default:
			return invalid("IPv6 literals need brackets")
		}
		if rest, ok := strings.CutPrefix(host, "*."); ok {
			p.wildcard, host = true, rest
		}
		if strings.Contains(host, "*") {
			return invalid("only one leading \"*.\" wildcard is allowed")
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			if p.wildcard || !addr.Is4() {
				return invalid("wildcards cannot apply to IP literals")
			}
			p.ip = addr
		} else {
			name, err := normalizeHostname(host)
			if err != nil {
				return invalid(err.Error())
			}
			if p.wildcard && !strings.Contains(name, ".") {
				return invalid("a wildcard needs a suffix of at least two labels")
			}
			p.host = name
		}
	}
	if port != "" {
		n, err := parsePort(port)
		if err != nil {
			return invalid(err.Error())
		}
		p.port = n
	}
	return p, nil
}

// String returns the canonical pattern.
func (p HostPattern) String() string {
	var host string
	switch {
	case p.ip.Is6():
		host = "[" + p.ip.String() + "]"
	case p.ip.IsValid():
		host = p.ip.String()
	case p.wildcard:
		host = "*." + p.host
	default:
		host = p.host
	}
	if p.port != 0 {
		host += ":" + strconv.Itoa(p.port)
	}
	return host
}

func parsePort(port string) (int, error) {
	for _, ch := range []byte(port) {
		if ch < '0' || ch > '9' {
			return 0, errors.New("port must be decimal")
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("port must be 1..65535")
	}
	return n, nil
}

// normalizeHostname lowercases an ASCII host name after removing one trailing
// dot. Non-ASCII names are refused rather than converted: Go's HTTP client
// maps them through IDNA, and matching a different conversion could admit a
// host the pattern did not name. Write internationalized names, in patterns
// and URLs, in their ASCII "xn--" form, which IDNA leaves unchanged. Empty or
// oversized labels and a numeric final label, which some resolvers read as an
// IPv4 address ("127.1", "0x7f.1"), are refused too.
func normalizeHostname(host string) (string, error) {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "", errors.New("empty host")
	}
	for i := 0; i < len(host); i++ {
		if host[i] >= utf8.RuneSelf {
			return "", errors.New("write internationalized host names in their ASCII xn-- form")
		}
	}
	ascii := strings.ToLower(host)
	if len(ascii) > 253 {
		return "", errors.New("host name exceeds 253 bytes")
	}
	labels := strings.Split(ascii, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return "", errors.New("host name labels must be 1..63 bytes")
		}
		for _, ch := range []byte(label) {
			if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
				return "", errors.New("host name labels contain only letters, digits and hyphens")
			}
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" || (strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == "") {
		return "", errors.New("a numeric final label is an address, not a host name")
	}
	return ascii, nil
}

// permits reports whether this policy lets use resolve the secret. HTTP uses
// require the destination the worker will connect to.
func (p *Policy) permits(use Use, target *url.URL) bool {
	if !slices.Contains(p.Uses, use) {
		return false
	}
	if use != UseHTTP {
		return true
	}
	return p.permitsURL(target)
}

func (p *Policy) permitsURL(target *url.URL) bool {
	if target == nil || target.User != nil || target.Opaque != "" {
		return false
	}
	var defaultPort int
	switch target.Scheme {
	case "https":
		defaultPort = 443
	case "http":
		if !p.PlainHTTP {
			return false
		}
		defaultPort = 80
	default:
		return false
	}
	port := defaultPort
	if raw := target.Port(); raw != "" {
		n, err := parsePort(raw)
		if err != nil {
			return false
		}
		port = n
	}
	host := target.Hostname()
	addr, ipErr := netip.ParseAddr(host)
	if ipErr == nil && addr.Zone() != "" {
		return false
	}
	name := ""
	if ipErr != nil {
		var err error
		if name, err = normalizeHostname(host); err != nil {
			return false
		}
	}
	for _, raw := range p.Hosts {
		pattern, err := ParseHostPattern(raw)
		if err != nil {
			return false
		}
		if (pattern.port == 0 && port != defaultPort) || (pattern.port != 0 && port != pattern.port) {
			continue
		}
		switch {
		case ipErr == nil:
			if pattern.ip.IsValid() && pattern.ip == addr {
				return true
			}
		case pattern.ip.IsValid():
		case pattern.wildcard:
			if strings.HasSuffix(name, "."+pattern.host) {
				return true
			}
		case name == pattern.host:
			return true
		}
	}
	return false
}
