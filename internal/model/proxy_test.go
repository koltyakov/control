package model

import (
	"strings"
	"testing"
	"time"
)

func TestValidateGatewayProxyMetadata(t *testing.T) {
	for _, proxy := range []*GatewayProxy{nil, {URL: "http://10.0.0.5:3128"}, {URL: "http://[fd00::5]:8080", Worker: strings.Repeat("a", 64), Managed: true}} {
		if err := ValidateGatewayProxy(proxy); err != nil {
			t.Fatal(proxy, err)
		}
	}
	for _, proxy := range []*GatewayProxy{
		{URL: ""}, {URL: "http://user:secret@10.0.0.5:3128"}, {URL: "http://10.0.0.5:3128/"},
		{URL: "http://10.0.0.5:3128?token=secret"}, {URL: "http://10.0.0.5:3128", Worker: strings.Repeat("a", 65)},
	} {
		if err := ValidateGatewayProxy(proxy); err == nil {
			t.Fatal("accepted unsafe or noncanonical route metadata", proxy)
		}
	}
}

func TestNormalizeProxyURL(t *testing.T) {
	for input, want := range map[string]string{
		"http://10.0.0.5:3128":        "http://10.0.0.5:3128",
		"http://10.0.0.5:3128/":       "http://10.0.0.5:3128",
		"http://Bridge.LAN.:8080":     "http://bridge.lan:8080",
		"http://[2001:DB8::5]:3128":   "http://[2001:db8::5]:3128",
		" http://[::ffff:10.0.0.5]:1": "http://10.0.0.5:1",
	} {
		if got, err := NormalizeProxyURL(input); err != nil || got != want {
			t.Errorf("%q: %q %v", input, got, err)
		}
	}
	for _, input := range []string{"", "10.0.0.5:3128", "socks5://10.0.0.5:1080", "http://10.0.0.5", "http://10.0.0.5:0", "http://10.0.0.5:03128", "http://u:p@10.0.0.5:3128", "http://10.0.0.5:3128?x", "http://10.0.0.5:3128#x", "http://[fe80::1%25en0]:3128", "http://[::]:3128", "http://224.0.0.1:3128"} {
		if _, err := NormalizeProxyURL(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestProxyStatusesSeparateConfigurationFromReadiness(t *testing.T) {
	now := time.Now()
	endpoints := []ProxyEndpoint{{URL: "http://10.0.0.5:3128"}, {URL: "http://10.0.0.6:3128"}, {URL: "http://10.0.0.7:3128"}}
	readiness := []ProxyReadiness{
		{URL: "http://10.0.0.5:3128", State: ProxyReady, CheckedAt: now, AgeSeconds: 5},
		{URL: "http://10.0.0.6:3128", State: ProxyReady, CheckedAt: now, AgeSeconds: int(ProxyReadinessTTL/time.Second) + 1},
	}
	got := ProxyStatuses(endpoints, true, readiness, now, now)
	if got[0].State != ProxyReady || got[1].State != ProxyStale || got[2].State != ProxyUnchecked {
		t.Fatalf("states %+v", got)
	}
	if !got[0].CheckedAt.Equal(now.Add(-5 * time.Second)) {
		t.Fatal("check time not dated on the observer's clock")
	}
	// A fresh report that arrived long ago is stale too.
	if ProxyStatuses(endpoints, true, readiness, now.Add(-ProxyReadinessTTL), now)[0].State != ProxyStale {
		t.Fatal("old report treated as ready")
	}
	for _, status := range ProxyStatuses(endpoints, false, readiness, now, now) {
		if status.State != ProxyOffline {
			t.Fatalf("offline worker %+v", status)
		}
	}
	if ValidProxyReadiness([]ProxyReadiness{{URL: "x", State: ProxyReady, CheckedAt: now, Error: "x"}}) || ValidProxyReadiness([]ProxyReadiness{{URL: "x", State: "maybe", CheckedAt: now}}) {
		t.Fatal("invalid readiness accepted")
	}
}
