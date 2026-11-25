package gateway

import (
	"bufio"
	"io"
	"math"
	"mime"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
)

// metricsTextContentType is the classic Prometheus text exposition format.
// Every Prometheus-compatible scraper accepts it, including those that prefer
// OpenMetrics, so the gateway does not implement a second text dialect.
const metricsTextContentType = "text/plain; version=0.0.4; charset=utf-8"

// metricsTextRequested selects the representation of GET /v1/admin/metrics.
// An explicit format query parameter wins; otherwise the Accept header must
// prefer a text exposition type over JSON. Missing, wildcard, and tied
// preferences keep the original JSON. Browser console requests always receive
// JSON: the console proxy forwards the query and Accept header, and the text
// representation is reserved for bearer-authenticated scrapers. ok is false
// for an unknown format value.
func metricsTextRequested(r *http.Request) (text, ok bool) {
	if r.Context().Value(consoleContextKey{}) != nil {
		return false, true
	}
	switch r.URL.Query().Get("format") {
	case "":
	case "json":
		return false, true
	case "prometheus":
		return true, true
	default:
		return false, false
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false, true
	}
	q := acceptQualities(accept)
	textQ := max(q.quality("text", "plain"), q.quality("application", "openmetrics-text"))
	return textQ > q.quality("application", "json"), true
}

type acceptRange struct {
	kind, subtype string
	q             float64
}

type acceptRanges []acceptRange

func acceptQualities(header string) acceptRanges {
	var ranges acceptRanges
	for part := range strings.SplitSeq(header, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		kind, subtype, found := strings.Cut(mediaType, "/")
		if !found {
			continue
		}
		q := 1.0
		if v, ok := params["q"]; ok {
			parsed, err := strconv.ParseFloat(v, 64)
			if err != nil || parsed < 0 || parsed > 1 || math.IsNaN(parsed) {
				continue
			}
			q = parsed
		}
		ranges = append(ranges, acceptRange{kind: kind, subtype: subtype, q: q})
	}
	return ranges
}

// quality returns the q-value of the most specific range matching a media
// type, as RFC 9110 section 12.5.1 describes, or zero when none matches.
func (ranges acceptRanges) quality(kind, subtype string) float64 {
	best, specificity := 0.0, -1
	for _, r := range ranges {
		s := -1
		switch {
		case r.kind == kind && r.subtype == subtype:
			s = 2
		case r.kind == kind && r.subtype == "*":
			s = 1
		case r.kind == "*" && r.subtype == "*":
			s = 0
		}
		if s > specificity || (s == specificity && s >= 0 && r.q > best) {
			best, specificity = r.q, s
		}
	}
	return best
}

// metricLabel is one label pair; label sets are fixed and bounded.
type metricLabel struct{ name, value string }

type metricSample struct {
	suffix string
	labels []metricLabel
	value  float64
}

// metricFamily is one exposition family. Families are written in slice order,
// so output is stable across scrapes.
type metricFamily struct {
	name, help, kind string
	samples          []metricSample
}

// writeMetricsText renders the same snapshot as the JSON representation.
// It exports only gateway-wide aggregates; no account, node, credential, or
// address appears as a label.
func (g *Gateway) writeMetricsText(w http.ResponseWriter, m GatewayMetrics) {
	software := g.options.Software
	if software.Version == "" {
		software = buildinfo.Current()
	}
	families := gatewayMetricFamilies(m, metricsBuild{
		Version:         software.Version,
		GatewayProtocol: software.GatewayProtocol,
		GOOS:            runtime.GOOS,
		GOARCH:          runtime.GOARCH,
		GoVersion:       runtime.Version(),
	}, g.startedAt)
	w.Header().Set("Content-Type", metricsTextContentType)
	w.Header().Set("Cache-Control", "no-store")
	_ = writeMetricFamilies(w, families)
}

// metricsBuild holds the build_info labels.
type metricsBuild struct {
	Version         string
	GatewayProtocol int
	GOOS, GOARCH    string
	GoVersion       string
}

func gauge(name, help string, value float64) metricFamily {
	return metricFamily{name: name, help: help, kind: "gauge", samples: []metricSample{{value: value}}}
}

func counter(name, help string, value int64) metricFamily {
	return metricFamily{name: name, help: help, kind: "counter", samples: []metricSample{{value: float64(value)}}}
}

func scoped(name, help string, gateway, account float64) metricFamily {
	return metricFamily{name: name, help: help, kind: "gauge", samples: []metricSample{
		{labels: []metricLabel{{"scope", "gateway"}}, value: gateway},
		{labels: []metricLabel{{"scope", "account"}}, value: account},
	}}
}

// gatewayMetricFamilies maps the snapshot to exposition families. Values are
// counters only when they never decrease during the process lifetime.
func gatewayMetricFamilies(m GatewayMetrics, build metricsBuild, started time.Time) []metricFamily {
	const p = "control_gateway_"
	l := m.Limits
	return []metricFamily{
		gauge(p+"active_handshakes", "Connections admitted but still completing their gateway handshake.", float64(m.ActiveHandshakes)),
		counter(p+"admission_rejects_total", "Connections refused by gateway or per-account admission limits.", m.AdmissionRejects),
		gauge(p+"admitted_connections", "Connections holding a gateway admission reservation, including handshakes.", float64(m.AdmittedConnections)),
		counter(p+"auth_failures_total", "Requests and connections presenting no valid credential.", m.AuthFailures),
		{name: p + "build_info", help: "Gateway build metadata. The value is always 1.", kind: "gauge", samples: []metricSample{{
			labels: []metricLabel{{"version", build.Version}, {"gateway_protocol", strconv.Itoa(build.GatewayProtocol)},
				{"goos", build.GOOS}, {"goarch", build.GOARCH}, {"goversion", build.GoVersion}},
			value: 1,
		}}},
		counter(p+"connects_total", "Node and client connections registered since startup.", m.Connects),
		counter(p+"disconnects_total", "Node and client connections closed since startup.", m.Disconnects),
		{name: p + "handshake_duration_seconds", help: "Time from admission to handshake completion, including failed attempts.", kind: "summary", samples: []metricSample{
			{suffix: "_sum", value: m.HandshakeDurationSeconds},
			{suffix: "_count", value: float64(m.HandshakeCount)},
		}},
		gauge(p+"limit_account_clients", "Configured client connection limit per account.", float64(l.ClientsPerAccount)),
		gauge(p+"limit_account_new_identities_per_minute", "Configured new identity registrations per account per minute.", float64(l.NewIdentitiesPerMinute)),
		gauge(p+"limit_account_nodes", "Configured node connection limit per account.", float64(l.NodesPerAccount)),
		scoped(p+"limit_connections", "Configured connection admission limit, gateway-wide or per account.", float64(l.Connections), float64(l.ConnectionsPerAccount)),
		scoped(p+"limit_handshakes", "Configured concurrent handshake limit, gateway-wide or per account.", float64(l.Handshakes), float64(l.HandshakesPerAccount)),
		scoped(p+"limit_queued_packet_bytes", "Configured retained packet byte budget, gateway-wide or per account.", float64(l.QueuedBytes), float64(l.QueuedBytesPerAccount)),
		counter(p+"new_identity_rejects_total", "Registrations refused by the per-account new identity rate.", m.NewIdentityRejects),
		counter(p+"packet_queue_rejects_total", "Packets refused by a gateway, account, or connection queue budget.", m.PacketQueueRejects),
		counter(p+"packet_write_failures_total", "Queued packet writes that failed on their connection.", m.PacketWriteFailures),
		gauge(p+"queued_packet_bytes", "Packet bytes currently retained in gateway queues, including writes in progress.", float64(m.QueuedPacketBytes)),
		gauge(p+"queued_packet_peak_bytes", "Highest retained packet bytes since startup.", float64(m.PeakQueuedPacketBytes)),
		gauge(p+"queued_relay_bytes", "Relay data payload bytes currently queued for destinations.", float64(m.QueuedRelayBytes)),
		counter(p+"relay_bytes_total", "Relay data payload bytes forwarded since startup.", m.RelayBytes),
		counter(p+"relay_dropped_packets_total", "Signaling packets dropped for a full destination queue.", m.RelayDropped),
		counter(p+"relay_overflows_total", "Relay sessions closed because their destination queue was full.", m.RelayOverflows),
		counter(p+"relay_packets_total", "Relay data packets forwarded since startup.", m.RelayPackets),
		{name: p + "routed_connections", help: "Registered connections currently routed by the gateway, by kind.", kind: "gauge", samples: []metricSample{
			{labels: []metricLabel{{"kind", "client"}}, value: float64(m.ConnectedClients)},
			{labels: []metricLabel{{"kind", "node"}}, value: float64(m.ConnectedMachines)},
		}},
		gauge(p+"start_time_seconds", "Gateway start time in seconds since the Unix epoch.", float64(started.UnixMilli())/1e3),
	}
}

func writeMetricFamilies(w io.Writer, families []metricFamily) error {
	b := bufio.NewWriter(w)
	for _, f := range families {
		_, _ = b.WriteString("# HELP " + f.name + " " + escapeMetricHelp(f.help) + "\n")
		_, _ = b.WriteString("# TYPE " + f.name + " " + f.kind + "\n")
		for _, s := range f.samples {
			_, _ = b.WriteString(f.name + s.suffix)
			if len(s.labels) > 0 {
				_ = b.WriteByte('{')
				for i, l := range s.labels {
					if i > 0 {
						_ = b.WriteByte(',')
					}
					_, _ = b.WriteString(l.name + `="` + escapeMetricLabel(l.value) + `"`)
				}
				_ = b.WriteByte('}')
			}
			_, _ = b.WriteString(" " + formatMetricValue(s.value) + "\n")
		}
	}
	return b.Flush()
}

var (
	metricHelpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	metricLabelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

func escapeMetricHelp(s string) string  { return metricHelpEscaper.Replace(s) }
func escapeMetricLabel(s string) string { return metricLabelEscaper.Replace(s) }

func formatMetricValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
