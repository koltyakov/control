package client

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// localAPIClient never routes local API requests through environment
// proxies, which would otherwise receive the bearer credential.
var localAPIClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Transport: transport}
}()

// SafeCredentialURL reports whether a node's implicit local API credential
// may be sent to raw: HTTPS anywhere, or plain HTTP to a loopback host.
// Other URLs need an explicitly supplied token.
func SafeCredentialURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
	default:
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
