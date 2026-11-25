package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/identity"
)

func metricsFixture() (GatewayMetrics, metricsBuild, time.Time) {
	m := GatewayMetrics{
		Limits: DefaultLimits(), AdmittedConnections: 3, ActiveHandshakes: 1,
		QueuedPacketBytes: 4096, PeakQueuedPacketBytes: 1 << 20,
		AdmissionRejects: 2, NewIdentityRejects: 1, PacketQueueRejects: 5,
		HandshakeCount: 4, HandshakeDurationSeconds: 0.375, PacketWriteFailures: 1,
		ConnectedMachines: 2, ConnectedClients: 1, QueuedRelayBytes: 512,
		Connects: 10, Disconnects: 7, AuthFailures: 3, RelayPackets: 100, RelayBytes: 123456,
		RelayOverflows: 1, RelayDropped: 2,
	}
	// Hostile build metadata exercises label escaping.
	build := metricsBuild{Version: "v1.2.3 \"rc\\1\"\nnext", GatewayProtocol: 1, GOOS: "linux", GOARCH: "amd64", GoVersion: "go1.27.4"}
	return m, build, time.Date(2026, 11, 24, 12, 0, 0, 500_000_000, time.UTC)
}

const metricsGolden = `# HELP control_gateway_active_handshakes Connections admitted but still completing their gateway handshake.
# TYPE control_gateway_active_handshakes gauge
control_gateway_active_handshakes 1
# HELP control_gateway_admission_rejects_total Connections refused by gateway or per-account admission limits.
# TYPE control_gateway_admission_rejects_total counter
control_gateway_admission_rejects_total 2
# HELP control_gateway_admitted_connections Connections holding a gateway admission reservation, including handshakes.
# TYPE control_gateway_admitted_connections gauge
control_gateway_admitted_connections 3
# HELP control_gateway_auth_failures_total Requests and connections presenting no valid credential.
# TYPE control_gateway_auth_failures_total counter
control_gateway_auth_failures_total 3
# HELP control_gateway_build_info Gateway build metadata. The value is always 1.
# TYPE control_gateway_build_info gauge
control_gateway_build_info{version="v1.2.3 \"rc\\1\"\nnext",gateway_protocol="1",goos="linux",goarch="amd64",goversion="go1.27.4"} 1
# HELP control_gateway_connects_total Node and client connections registered since startup.
# TYPE control_gateway_connects_total counter
control_gateway_connects_total 10
# HELP control_gateway_disconnects_total Node and client connections closed since startup.
# TYPE control_gateway_disconnects_total counter
control_gateway_disconnects_total 7
# HELP control_gateway_handshake_duration_seconds Time from admission to handshake completion, including failed attempts.
# TYPE control_gateway_handshake_duration_seconds summary
control_gateway_handshake_duration_seconds_sum 0.375
control_gateway_handshake_duration_seconds_count 4
# HELP control_gateway_limit_account_clients Configured client connection limit per account.
# TYPE control_gateway_limit_account_clients gauge
control_gateway_limit_account_clients 64
# HELP control_gateway_limit_account_new_identities_per_minute Configured new identity registrations per account per minute.
# TYPE control_gateway_limit_account_new_identities_per_minute gauge
control_gateway_limit_account_new_identities_per_minute 120
# HELP control_gateway_limit_account_nodes Configured node connection limit per account.
# TYPE control_gateway_limit_account_nodes gauge
control_gateway_limit_account_nodes 192
# HELP control_gateway_limit_connections Configured connection admission limit, gateway-wide or per account.
# TYPE control_gateway_limit_connections gauge
control_gateway_limit_connections{scope="gateway"} 1024
control_gateway_limit_connections{scope="account"} 256
# HELP control_gateway_limit_handshakes Configured concurrent handshake limit, gateway-wide or per account.
# TYPE control_gateway_limit_handshakes gauge
control_gateway_limit_handshakes{scope="gateway"} 64
control_gateway_limit_handshakes{scope="account"} 16
# HELP control_gateway_limit_queued_packet_bytes Configured retained packet byte budget, gateway-wide or per account.
# TYPE control_gateway_limit_queued_packet_bytes gauge
control_gateway_limit_queued_packet_bytes{scope="gateway"} 67108864
control_gateway_limit_queued_packet_bytes{scope="account"} 16777216
# HELP control_gateway_new_identity_rejects_total Registrations refused by the per-account new identity rate.
# TYPE control_gateway_new_identity_rejects_total counter
control_gateway_new_identity_rejects_total 1
# HELP control_gateway_packet_queue_rejects_total Packets refused by a gateway, account, or connection queue budget.
# TYPE control_gateway_packet_queue_rejects_total counter
control_gateway_packet_queue_rejects_total 5
# HELP control_gateway_packet_write_failures_total Queued packet writes that failed on their connection.
# TYPE control_gateway_packet_write_failures_total counter
control_gateway_packet_write_failures_total 1
# HELP control_gateway_queued_packet_bytes Packet bytes currently retained in gateway queues, including writes in progress.
# TYPE control_gateway_queued_packet_bytes gauge
control_gateway_queued_packet_bytes 4096
# HELP control_gateway_queued_packet_peak_bytes Highest retained packet bytes since startup.
# TYPE control_gateway_queued_packet_peak_bytes gauge
control_gateway_queued_packet_peak_bytes 1048576
# HELP control_gateway_queued_relay_bytes Relay data payload bytes currently queued for destinations.
# TYPE control_gateway_queued_relay_bytes gauge
control_gateway_queued_relay_bytes 512
# HELP control_gateway_relay_bytes_total Relay data payload bytes forwarded since startup.
# TYPE control_gateway_relay_bytes_total counter
control_gateway_relay_bytes_total 123456
# HELP control_gateway_relay_dropped_packets_total Signaling packets dropped for a full destination queue.
# TYPE control_gateway_relay_dropped_packets_total counter
control_gateway_relay_dropped_packets_total 2
# HELP control_gateway_relay_overflows_total Relay sessions closed because their destination queue was full.
# TYPE control_gateway_relay_overflows_total counter
control_gateway_relay_overflows_total 1
# HELP control_gateway_relay_packets_total Relay data packets forwarded since startup.
# TYPE control_gateway_relay_packets_total counter
control_gateway_relay_packets_total 100
# HELP control_gateway_routed_connections Registered connections currently routed by the gateway, by kind.
# TYPE control_gateway_routed_connections gauge
control_gateway_routed_connections{kind="client"} 1
control_gateway_routed_connections{kind="node"} 2
# HELP control_gateway_start_time_seconds Gateway start time in seconds since the Unix epoch.
# TYPE control_gateway_start_time_seconds gauge
control_gateway_start_time_seconds 1795521600.5
`

func renderMetricsFixture(t *testing.T) string {
	t.Helper()
	m, build, started := metricsFixture()
	var b bytes.Buffer
	if err := writeMetricFamilies(&b, gatewayMetricFamilies(m, build, started)); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestMetricsTextGolden(t *testing.T) {
	got := renderMetricsFixture(t)
	if got != metricsGolden {
		t.Fatalf("rendered metrics differ from golden:\n%s", got)
	}
	if again := renderMetricsFixture(t); again != got {
		t.Fatal("metrics output is not stable across renders")
	}
	validateExposition(t, got)
}

func TestMetricsTextValidatorRejectsMalformedExposition(t *testing.T) {
	for name, text := range map[string]string{
		"bad name":         "# TYPE 1bad gauge\n1bad 1\n",
		"bad label":        "# TYPE a gauge\na{__x=\"1\"} 1\n",
		"bad escape":       "# TYPE a gauge\na{x=\"\\t\"} 1\n",
		"unquoted":         "# TYPE a gauge\na{x=1} 1\n",
		"bad value":        "# TYPE a gauge\na one\n",
		"no type":          "a 1\n",
		"duplicate series": "# TYPE a gauge\na{x=\"1\"} 1\na{x=\"1\"} 2\n",
		"type mismatch":    "# TYPE a gauge\nb 1\n",
		"counter suffix":   "# TYPE a counter\na 1\n",
		"repeated family":  "# TYPE a gauge\na 1\n# TYPE b gauge\nb 1\n# TYPE a gauge\n",
		"missing newline":  "# TYPE a gauge\na 1",
		"help after type":  "# TYPE a gauge\n# HELP a x\na 1\n",
	} {
		if err := checkExposition(text); err == nil {
			t.Errorf("%s: validator accepted %q", name, text)
		}
	}
}

func validateExposition(t *testing.T, text string) {
	t.Helper()
	if err := checkExposition(text); err != nil {
		t.Fatal(err)
	}
}

var (
	metricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNamePattern  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// checkExposition is a minimal validator for the classic Prometheus text
// format, strict enough to catch the mistakes a hand-written encoder makes.
func checkExposition(text string) error {
	if !strings.HasSuffix(text, "\n") {
		return fmt.Errorf("exposition must end with a newline")
	}
	types, helps, series := map[string]string{}, map[string]bool{}, map[string]bool{}
	current := ""
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		fail := func(format string, args ...any) error {
			return fmt.Errorf("line %d %q: %s", i+1, line, fmt.Sprintf(format, args...))
		}
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			keyword, rest, _ := strings.Cut(rest, " ")
			name, value, _ := strings.Cut(rest, " ")
			switch keyword {
			case "HELP":
				if !metricNamePattern.MatchString(name) || helps[name] || types[name] != "" {
					return fail("invalid, repeated, or late HELP")
				}
				if err := checkEscapes(value, false); err != nil {
					return fail("%v", err)
				}
				helps[name] = true
			case "TYPE":
				if !metricNamePattern.MatchString(name) || types[name] != "" {
					return fail("invalid or repeated TYPE")
				}
				if !slices.Contains([]string{"counter", "gauge", "summary", "histogram", "untyped"}, value) {
					return fail("unknown type")
				}
				if value == "counter" && !strings.HasSuffix(name, "_total") {
					return fail("counter name lacks _total")
				}
				types[name], current = value, name
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			return fail("unexpected blank or comment line")
		}
		name, labels, value, err := splitSample(line)
		if err != nil {
			return fail("%v", err)
		}
		if !metricNamePattern.MatchString(name) {
			return fail("invalid metric name")
		}
		if current == "" {
			return fail("sample without TYPE")
		}
		switch types[current] {
		case "summary":
			if name != current+"_sum" && name != current+"_count" && name != current {
				return fail("sample outside summary family %s", current)
			}
		default:
			if name != current {
				return fail("sample outside family %s", current)
			}
		}
		key, err := checkLabels(labels)
		if err != nil {
			return fail("%v", err)
		}
		if series[name+key] {
			return fail("duplicate series")
		}
		series[name+key] = true
		v, err := parseSampleValue(value)
		if err != nil {
			return fail("%v", err)
		}
		if (types[current] == "counter" || strings.HasSuffix(name, "_count")) && v < 0 {
			return fail("negative counter")
		}
	}
	return nil
}

func splitSample(line string) (name, labels, value string, err error) {
	if open := strings.IndexByte(line, '{'); open >= 0 && open < strings.IndexByte(line, ' ') {
		name = line[:open]
		// Find the closing brace outside quoted label values.
		quoted, escaped := false, false
		for j := open + 1; j < len(line); j++ {
			c := line[j]
			switch {
			case escaped:
				escaped = false
			case c == '\\' && quoted:
				escaped = true
			case c == '"':
				quoted = !quoted
			case c == '}' && !quoted:
				labels, value = line[open+1:j], line[j+1:]
				if !strings.HasPrefix(value, " ") {
					return "", "", "", fmt.Errorf("missing space before value")
				}
				return name, labels, value[1:], nil
			}
		}
		return "", "", "", fmt.Errorf("unterminated label set")
	}
	name, value, ok := strings.Cut(line, " ")
	if !ok {
		return "", "", "", fmt.Errorf("missing value")
	}
	return name, "", value, nil
}

// checkLabels validates name="value" pairs and returns a canonical series key.
func checkLabels(labels string) (string, error) {
	if labels == "" {
		return "", nil
	}
	var pairs []string
	seen := map[string]bool{}
	for rest := labels; rest != ""; {
		name, after, ok := strings.Cut(rest, "=")
		if !ok || !labelNamePattern.MatchString(name) || strings.HasPrefix(name, "__") || seen[name] {
			return "", fmt.Errorf("invalid or repeated label name %q", name)
		}
		seen[name] = true
		if !strings.HasPrefix(after, `"`) {
			return "", fmt.Errorf("unquoted label value")
		}
		end, escaped := -1, false
		for j := 1; j < len(after); j++ {
			if escaped {
				escaped = false
				continue
			}
			if after[j] == '\\' {
				escaped = true
			} else if after[j] == '"' {
				end = j
				break
			}
		}
		if end < 0 {
			return "", fmt.Errorf("unterminated label value")
		}
		if err := checkEscapes(after[1:end], true); err != nil {
			return "", err
		}
		pairs = append(pairs, name+"="+after[:end+1])
		rest = after[end+1:]
		if rest != "" {
			if rest, ok = strings.CutPrefix(rest, ","); !ok || rest == "" {
				return "", fmt.Errorf("invalid label separator")
			}
		}
	}
	slices.Sort(pairs)
	return "{" + strings.Join(pairs, ",") + "}", nil
}

// checkEscapes allows only \\ and \n, plus \" inside label values, and
// rejects raw newlines and unescaped quotes in label values.
func checkEscapes(s string, label bool) error {
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case '\n':
			return fmt.Errorf("raw newline")
		case '"':
			if label {
				return fmt.Errorf("unescaped quote")
			}
		case '\\':
			if j+1 >= len(s) {
				return fmt.Errorf("dangling escape")
			}
			j++
			if s[j] != '\\' && s[j] != 'n' && (!label || s[j] != '"') {
				return fmt.Errorf("invalid escape \\%c", s[j])
			}
		}
	}
	return nil
}

func parseSampleValue(value string) (float64, error) {
	value, _, _ = strings.Cut(value, " ") // optional timestamp
	switch value {
	case "NaN", "+Inf", "-Inf":
		return 0, nil
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", value)
	}
	return v, nil
}

func metricsRequest(t *testing.T, url, key, accept string) (int, string, string) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

func TestMetricsContentNegotiationAndDefaultJSON(t *testing.T) {
	g, s := limitedGateway(t, DefaultLimits())
	endpoint := s.URL + "/v1/admin/metrics"
	// The gateway is idle, so the snapshot is stable between requests.
	var want bytes.Buffer
	if err := json.NewEncoder(&want).Encode(g.metricsSnapshot()); err != nil {
		t.Fatal(err)
	}
	prometheusAccept := "application/openmetrics-text;version=1.0.0;q=0.5,application/openmetrics-text;version=0.0.1;q=0.4,text/plain;version=0.0.4;q=0.3,*/*;q=0.2"
	for _, c := range []struct {
		query, accept string
		text          bool
	}{
		{"", "", false},
		{"", "*/*", false},
		{"", "application/json", false},
		{"", "application/json, text/plain;q=0.5", false},
		{"", "text/plain;q=0, */*", false},
		{"", "text/plain, application/json", false},
		{"", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", false},
		{"?format=json", "text/plain", false},
		{"", "text/plain", true},
		{"", "text/plain;version=0.0.4", true},
		{"", "application/openmetrics-text", true},
		{"", prometheusAccept, true},
		{"", "*/*;q=0.1, text/*", true},
		{"?format=prometheus", "", true},
		{"?format=prometheus", "application/json", true},
	} {
		status, contentType, body := metricsRequest(t, endpoint+c.query, superKey, c.accept)
		if status != http.StatusOK {
			t.Fatalf("%q %q: HTTP %d", c.query, c.accept, status)
		}
		if c.text {
			if contentType != metricsTextContentType || !strings.Contains(body, "# TYPE control_gateway_connects_total counter\n") {
				t.Fatalf("%q %q: expected text exposition, got %s: %s", c.query, c.accept, contentType, body)
			}
			validateExposition(t, body)
			continue
		}
		if contentType != "application/json" || body != want.String() {
			t.Fatalf("%q %q: default JSON changed: %s %s, want %s", c.query, c.accept, contentType, body, want.String())
		}
	}
	if status, _, _ := metricsRequest(t, endpoint+"?format=xml", superKey, ""); status != http.StatusBadRequest {
		t.Fatalf("unknown format: HTTP %d", status)
	}
}

func TestMetricsTextAuthorizationAndNoIdentityLabels(t *testing.T) {
	g, s := installationFixture(t)
	alice, account := createTestUser(t, s, "alice-private-account")
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rawPeer(t, s.URL, account.Key, "secret-node-name", alice.ID, id) == nil {
		t.Fatal("node registration failed")
	}
	endpoint := s.URL + "/v1/admin/metrics?format=prometheus"
	for _, key := range []string{"", "wrong-key", commonKey, account.Key} {
		for _, accept := range []string{"", "text/plain"} {
			status, _, body := metricsRequest(t, endpoint, key, accept)
			if status != http.StatusForbidden || strings.Contains(body, "control_gateway_") {
				t.Fatalf("non-superuser credential read metrics: HTTP %d %s", status, body)
			}
		}
	}
	status, _, body := metricsRequest(t, endpoint, superKey, "")
	if status != http.StatusOK {
		t.Fatalf("superuser: HTTP %d", status)
	}
	validateExposition(t, body)
	if !strings.Contains(body, "control_gateway_routed_connections{kind=\"node\"} 1\n") {
		t.Fatal("connected node not counted", body)
	}
	g.mu.Lock()
	keyIDs := make([]string, 0, len(g.keys))
	for keyID := range g.keys {
		keyIDs = append(keyIDs, keyID)
	}
	g.mu.Unlock()
	for _, secret := range append(keyIDs, "alice-private-account", alice.ID, "secret-node-name", id.ID, base64.StdEncoding.EncodeToString(id.Public), account.Key, commonKey, superKey, "127.0.0.1") {
		if secret != "" && strings.Contains(body, secret) {
			t.Fatalf("metrics expose identity %q", secret)
		}
	}
	// Only the fixed, bounded label names appear.
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "#") || !strings.Contains(line, "{") {
			continue
		}
		for _, label := range regexp.MustCompile(`([a-z_]+)="`).FindAllStringSubmatch(line, -1) {
			if !slices.Contains([]string{"scope", "kind", "version", "gateway_protocol", "goos", "goarch", "goversion"}, label[1]) {
				t.Fatalf("unexpected label %q in %s", label[1], line)
			}
		}
	}
}

func TestConsoleMetricsStayJSON(t *testing.T) {
	_, s := installationFixture(t)
	provisionBrowser(t, testAdmin{URL: s.URL, Key: superKey}, "")
	operator := browserLogin(t, s.URL, "@operator", browserPassword)
	for _, c := range []struct{ query, accept string }{{"", ""}, {"?format=prometheus", ""}, {"", "text/plain"}, {"?format=prometheus", "text/plain"}} {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL+"/console/api/admin/metrics"+c.query, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.AddCookie(operator.Cookie)
		if c.accept != "" {
			r.Header.Set("Accept", c.accept)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		var metrics GatewayMetrics
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" || json.Unmarshal(body, &metrics) != nil || metrics.Limits != DefaultLimits() {
			t.Fatalf("%q %q: console metrics not JSON: HTTP %d %s %s", c.query, c.accept, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
}
