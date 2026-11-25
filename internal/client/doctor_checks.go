package client

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

const doctorLoginHint = "run control login --gateway URL (docs/installation.md#log-in-once)"

func (d *doctorRun) role() string {
	if d.authErr != nil {
		return ""
	}
	return d.auth.Role
}

// owner reports an account or superuser key. Hints naming fleet-management
// commands are shown only to these roles, matching role-scoped help.
func (d *doctorRun) owner() bool { return d.role() == "user" || d.role() == "superuser" }

func (d *doctorRun) gatewayDisplay() string {
	u, err := url.Parse(d.opts.Gateway.URL)
	if err != nil || u.Host == "" {
		return "the configured gateway"
	}
	return u.Redacted()
}

func (d *doctorRun) loginCheck() DoctorCheck {
	c := DoctorCheck{Check: "login"}
	gw := d.opts.Gateway
	source := d.opts.LoginSource
	if source == "" {
		source = "the configured credential"
	}
	switch {
	case gw.URL == "":
		c.Status, c.Detail, c.Hint = DoctorFailed, "no gateway login is configured", doctorLoginHint
	case gw.Key == "":
		c.Status, c.Detail, c.Hint = DoctorFailed, "gateway "+d.gatewayDisplay()+" is configured, but no credential is saved for it", doctorLoginHint
	case doctorHTTPStatus(d.authErr) == 401:
		c.Status, c.Detail, c.Hint = DoctorFailed, "the gateway rejected "+source+"; it may be revoked or belong to another gateway", "run control login --gateway "+d.gatewayDisplay()+" with a current key"
	case d.authErr != nil:
		c.Status, c.Detail = DoctorSkipped, "credential from "+source+" is present; its role is unknown because the gateway check did not complete"
	default:
		c.Status = DoctorOK
		switch d.auth.Role {
		case "superuser":
			c.Detail = "superuser key from " + source
		case "user":
			c.Detail = "account key from " + source
			if d.auth.Username != "" {
				c.Detail += " (user " + d.auth.Username + ")"
			}
		case "orchestrator":
			c.Detail = "orchestrator key from " + source + "; it permits account-client execution, not fleet administration"
		case "common":
			c.Detail = "common key from " + source + "; it permits discovery and gateway status, not account-client execution"
		default:
			c.Detail = "credential from " + source + " authenticates as " + d.auth.Role
		}
	}
	return c
}

func (d *doctorRun) gatewayCheck() DoctorCheck {
	c := DoctorCheck{Check: "gateway"}
	if !d.hasLogin() {
		c.Status, c.Detail = DoctorSkipped, "no gateway login"
		return c
	}
	switch code := doctorHTTPStatus(d.authErr); {
	case d.authErr != nil && doctorTimedOut(d.authErr):
		c.Status, c.Detail = DoctorFailed, d.gatewayDisplay()+" did not answer /v1/auth: "+doctorErr(d.authErr)
		c.Hint = "check the gateway URL and this host's network; proxy settings appear under environment"
		return c
	case code == 401 || code == 403:
		c.Status, c.Detail, c.Hint = DoctorFailed, d.gatewayDisplay()+" is reachable but refused authentication (HTTP "+fmt.Sprint(code)+")", doctorLoginHint
		return c
	case d.authErr != nil:
		c.Status, c.Detail = DoctorFailed, "cannot reach "+d.gatewayDisplay()+": "+doctorErr(d.authErr)
		c.Hint = "check the gateway URL and this host's network; proxy settings appear under environment (docs/gateway.md)"
		return c
	}
	c.Status = DoctorOK
	c.Detail = d.gatewayDisplay() + " is reachable and authenticated"
	if g := d.status.pool.Gateway; d.statusErr == nil && g != nil {
		version := g.Software.Version
		if version == "" {
			version = "unknown"
		}
		c.Detail += "; gateway version " + version
	} else if d.statusErr != nil {
		c.Status = DoctorWarning
		c.Detail += "; gateway status is unavailable: " + doctorErr(d.statusErr)
	}
	if d.auth.GatewayProtocol > 0 {
		c.Detail += fmt.Sprintf(", protocol epoch %d", d.auth.GatewayProtocol)
	}
	if !d.auth.Delegation {
		c.Status = DoctorFailed
		c.Detail += "; it predates instruction-bound delegation, so account clients refuse to send work through it"
		c.Hint = d.gatewayUpgradeHint()
	}
	return c
}

func (d *doctorRun) gatewayUpgradeHint() string {
	if d.role() == "superuser" {
		return "upgrade the gateway (docs/gateway.md#maintenance)"
	}
	return "ask the gateway operator to upgrade the gateway"
}

func (d *doctorRun) workerUpgradeHint() string {
	switch d.role() {
	case "superuser":
		return "control update status shows rollout progress; managed nodes follow the gateway's selected release (docs/updates.md)"
	case "user":
		return "managed nodes follow the gateway's selected release; ask the gateway operator to publish a newer one (docs/updates.md)"
	}
	return "ask the fleet owner to update this machine (docs/updates.md)"
}

func doctorVersion(v string) string {
	if v == "" {
		return "version unknown"
	}
	return v
}

// doctorCompare compares release versions; development builds and other
// unparsable versions are not compared.
func doctorCompare(a, b string) (int, bool) {
	_, okA := update.ParseReleaseVersion(a)
	_, okB := update.ParseReleaseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	return update.CompareReleaseVersions(a, b), true
}

func (d *doctorRun) selectedRelease() string {
	if d.selectionErr != nil {
		return ""
	}
	return d.selection.Manifest.Version
}

func (d *doctorRun) gatewayVersion() string {
	if d.statusErr != nil || d.status.pool.Gateway == nil {
		return ""
	}
	return d.status.pool.Gateway.Software.Version
}

func (d *doctorRun) versionCheck() DoctorCheck {
	c := DoctorCheck{Check: "version", Status: DoctorOK}
	client := d.software.Version
	c.Detail = "client " + doctorVersion(client) + " (" + d.software.OS + "/" + d.software.Arch + ")"
	if !d.hasLogin() || d.authErr != nil || d.statusErr != nil || d.status.pool.Gateway == nil {
		c.Status = DoctorSkipped
		c.Detail += "; gateway version unavailable"
		return c
	}
	gateway := d.gatewayVersion()
	c.Detail += ", gateway " + doctorVersion(gateway)
	release := d.selectedRelease()
	if release != "" {
		c.Detail += ", fleet release " + release
	}
	var notes, hints []string
	if order, ok := doctorCompare(client, gateway); !ok {
		notes = append(notes, "development or unparsable versions are not compared")
	} else if order < 0 {
		c.Status = DoctorWarning
		notes = append(notes, "this client is older than the gateway")
		hints = append(hints, "control update installs the latest CLI (docs/updates.md#update-the-local-cli)")
	}
	if order, ok := doctorCompare(client, release); ok && order < 0 {
		c.Status = DoctorWarning
		notes = append(notes, "this client is older than the fleet's selected release")
		hints = append(hints, "control update --version "+release+" matches the fleet release")
	}
	// Clients are exempt from the node range; a node started from this binary is not.
	if epoch := d.auth.GatewayProtocol; d.opts.LocalNode != "" && d.software.NodeGatewayMax > 0 && !d.software.SupportsGateway(epoch) {
		c.Status = DoctorWarning
		notes = append(notes, fmt.Sprintf("a node started from this binary supports gateway protocol %d..%d, not %d", d.software.NodeGatewayMin, d.software.NodeGatewayMax, epoch))
		hints = append(hints, "control update")
	}
	if len(notes) > 0 {
		c.Detail += "; " + strings.Join(notes, "; ")
	}
	c.Hint = strings.Join(doctorUnique(hints), "; ")
	return c
}

func doctorUnique(values []string) []string {
	var out []string
	for _, v := range values {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func (d *doctorRun) clockCheck() DoctorCheck {
	c := DoctorCheck{Check: "clock"}
	if !d.hasLogin() {
		c.Status, c.Detail = DoctorSkipped, "no gateway login"
		return c
	}
	if d.statusErr != nil {
		c.Status, c.Detail = DoctorSkipped, "gateway status is unavailable"
		return c
	}
	s := d.status
	rtt := s.received.Sub(s.sent)
	uncertainty := rtt / 2
	mid := s.sent.Add(uncertainty)
	var offset time.Duration
	switch {
	case !s.pool.ObservedAt.IsZero():
		offset = s.pool.ObservedAt.Sub(mid)
	case !s.date.IsZero():
		// Date has one-second resolution: the true time lies in [Date, Date+1s).
		offset = s.date.Add(500 * time.Millisecond).Sub(mid)
		uncertainty += 500 * time.Millisecond
	default:
		c.Status, c.Detail = DoctorSkipped, "the gateway response carried no time"
		return c
	}
	magnitude := max(offset, -offset)
	excess := max(magnitude-uncertainty, 0)
	precision := max(uncertainty.Round(time.Millisecond), time.Millisecond)
	if excess < time.Millisecond {
		c.Status, c.Detail = DoctorOK, fmt.Sprintf("this host's clock matches the gateway within ±%s", precision)
		return c
	}
	direction := "behind"
	if offset < 0 {
		direction = "ahead of"
	}
	c.Detail = fmt.Sprintf("this host's clock is %s %s the gateway (±%s)", magnitude.Round(time.Millisecond), direction, precision)
	switch {
	case excess >= doctorSkewFailure:
		c.Status = DoctorFailed
		c.Detail += "; 30-second signed requests such as remote secret writes will be refused"
	case excess > doctorSkewWarning:
		c.Status = DoctorWarning
		c.Detail += "; expiring grants and 30-second secret envelopes lose margin"
	default:
		c.Status = DoctorOK
		return c
	}
	c.Hint = "enable time synchronization (NTP) on this host and the gateway host"
	return c
}

func (d *doctorRun) localCheck() DoctorCheck {
	c := DoctorCheck{Check: "local-node"}
	configured := d.opts.LocalNode != ""
	address := d.opts.LocalAPI
	switch {
	case d.localErr == nil:
		c.Status, c.Detail = DoctorOK, "local node API answers at "+address
		if configured {
			c.Detail = "local node " + d.opts.LocalNode + " answers at " + address
		}
		c.Detail += "; remote commands still use the account client session"
	case errors.Is(d.localErr, errDoctorNoLocalAPI) || connectionRefused(d.localErr):
		if !configured {
			c.Status, c.Detail = DoctorOK, "no local node is running; this client works standalone through the gateway"
			return c
		}
		c.Status = DoctorWarning
		c.Detail = "this host's node profile " + d.opts.LocalNode + " is configured, but nothing answers at " + address
		c.Hint = "control service status shows the background node; control service start starts it"
	case doctorTimedOut(d.localErr):
		c.Status, c.Detail = DoctorWarning, "local node API at "+address+": "+doctorErr(d.localErr)
		c.Hint = "control service status shows the background node"
	default:
		c.Status, c.Detail = DoctorWarning, "local node API at "+address+" answered with an error: "+doctorErr(d.localErr)
		c.Hint = "the API accepts this host's node token; check --api, --token and CONTROL_API"
	}
	return c
}

// doctorProxy describes the environment proxy for one URL, mirroring the
// variables net/http's default transport reads. It never returns userinfo.
type doctorProxy struct {
	variable, host string
	excluded       bool
}

func doctorEnvProxy(getenv func(string) string, raw string) (doctorProxy, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return doctorProxy{}, false
	}
	names := []string{"HTTP_PROXY", "http_proxy"}
	if strings.EqualFold(u.Scheme, "https") {
		names = []string{"HTTPS_PROXY", "https_proxy"}
	}
	var p doctorProxy
	value := ""
	for _, name := range names {
		if value = getenv(name); value != "" {
			p.variable = name
			break
		}
	}
	if value == "" {
		return doctorProxy{}, false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback() {
		return doctorProxy{}, false
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	if proxy, err := url.Parse(value); err == nil {
		p.host = proxy.Host
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	noProxy := getenv("NO_PROXY")
	if noProxy == "" {
		noProxy = getenv("no_proxy")
	}
	p.excluded = doctorNoProxy(noProxy, host, port)
	return p, true
}

func doctorNoProxy(list, host, port string) bool {
	ip := net.ParseIP(host)
	for _, entry := range strings.Split(list, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		if _, network, err := net.ParseCIDR(entry); err == nil {
			if ip != nil && network.Contains(ip) {
				return true
			}
			continue
		}
		if h, p, err := net.SplitHostPort(entry); err == nil {
			if p != port {
				continue
			}
			entry = h
		}
		if entryIP := net.ParseIP(entry); entryIP != nil {
			if ip != nil && entryIP.Equal(ip) {
				return true
			}
			continue
		}
		entry = strings.TrimPrefix(entry, "*")
		if strings.HasPrefix(entry, ".") {
			if strings.HasSuffix(host, entry) {
				return true
			}
			continue
		}
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

func (d *doctorRun) environmentCheck() DoctorCheck {
	c := DoctorCheck{Check: "environment", Status: DoctorOK}
	gw := d.opts.Gateway
	if gw.URL == "" {
		c.Status, c.Detail = DoctorSkipped, "no gateway URL to evaluate"
		return c
	}
	var notes, hints []string
	gatewayUnreachable := d.hasLogin() && d.authErr != nil && doctorHTTPStatus(d.authErr) == 0
	if gw.Route.Proxied() {
		host := "the configured proxy"
		if u, err := url.Parse(gw.Route.Proxy()); err == nil && u.Host != "" {
			host = u.Host
		}
		notes = append(notes, "gateway traffic uses this profile's explicit gateway proxy "+host+"; environment proxy variables do not apply to it")
		if gatewayUnreachable {
			c.Status = DoctorWarning
			hints = append(hints, "the gateway proxy worker must be online and able to reach the gateway (docs/gateway-proxy.md)")
		}
	} else if p, ok := doctorEnvProxy(d.opts.Getenv, gw.URL); ok {
		if p.excluded {
			notes = append(notes, p.variable+" is set, but NO_PROXY excludes the gateway host")
		} else {
			notes = append(notes, "gateway requests go through "+p.host+" from "+p.variable)
			if gatewayUnreachable {
				c.Status = DoctorWarning
				hints = append(hints, "if that proxy cannot reach the gateway, add the gateway host to NO_PROXY or unset "+p.variable+" for Control")
			}
		}
	}
	if u, err := url.Parse(gw.URL); err == nil && strings.EqualFold(u.Scheme, "http") && !SafeCredentialURL(gw.URL) {
		c.Status = DoctorWarning
		notes = append(notes, "the gateway URL is plain HTTP on a non-loopback host, so credentials and fleet metadata travel unencrypted")
		hints = append(hints, "serve the gateway over HTTPS (docs/gateway.md#5-enable-public-https) and log in with the https:// URL")
	}
	if len(notes) == 0 {
		notes = append(notes, "no proxy variables affect the gateway, and its URL uses HTTPS or loopback")
	}
	c.Detail = strings.Join(notes, "; ")
	c.Hint = strings.Join(hints, "; ")
	return c
}

func (d *doctorRun) statusEntry(id string) *model.NodeActivitySnapshot {
	if d.statusErr != nil {
		return nil
	}
	for i := range d.status.pool.Nodes {
		if d.status.pool.Nodes[i].ID == id {
			return &d.status.pool.Nodes[i]
		}
	}
	return nil
}

func doctorWhen(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return t.UTC().Format(time.RFC3339)
}

func doctorPlatform(os, arch string) string {
	if arch == "" {
		return os
	}
	return os + "/" + arch
}

// fleetChecks summarizes each machine from owner-visible gateway status only.
func (d *doctorRun) fleetChecks() []DoctorCheck {
	c := DoctorCheck{Check: "fleet"}
	switch {
	case !d.hasLogin():
		c.Status, c.Detail = DoctorSkipped, "no gateway login"
		return []DoctorCheck{c}
	case d.authErr != nil:
		c.Status, c.Detail = DoctorSkipped, "the gateway check did not complete"
		return []DoctorCheck{c}
	case d.statusErr != nil:
		c.Status, c.Detail = DoctorFailed, "cannot read gateway status: "+doctorErr(d.statusErr)
		c.Hint = "control dashboard --once shows the same gateway status once it is reachable"
		return []DoctorCheck{c}
	}
	nodes := slices.Clone(d.status.pool.Nodes)
	slices.SortFunc(nodes, func(a, b model.NodeActivitySnapshot) int { return strings.Compare(a.Name, b.Name) })
	online := 0
	for _, n := range nodes {
		if n.Online {
			online++
		}
	}
	c.Status = DoctorOK
	c.Detail = fmt.Sprintf("%d registered machines, %d online; run control doctor NAME for peer checks", len(nodes), online)
	checks := []DoctorCheck{c}
	gateway := d.gatewayVersion()
	for i, n := range nodes {
		if i == doctorFleetLimit {
			checks[0].Detail += fmt.Sprintf("; %d more machines omitted", len(nodes)-doctorFleetLimit)
			break
		}
		line := DoctorCheck{Check: "machine:" + n.Name, Status: DoctorOK}
		var facts []string
		switch {
		case n.Status == "update":
			facts = append(facts, "managed update in progress")
			if n.UpdateState != "" {
				facts[len(facts)-1] += " (" + n.UpdateState + ")"
			}
		case n.Online:
			facts = append(facts, "online")
		default:
			line.Status = DoctorWarning
			facts = append(facts, "offline since "+doctorWhen(n.LastSeen))
		}
		if n.Disabled {
			line.Status = DoctorWarning
			facts = append(facts, "disabled")
		}
		if n.ControlPending {
			line.Status = DoctorWarning
			facts = append(facts, "policy change not yet acknowledged")
		}
		facts = append(facts, doctorVersion(n.Software.Version), doctorPlatform(n.OS, n.Software.Arch))
		if order, ok := doctorCompare(n.Software.Version, gateway); ok && order < 0 {
			line.Status = DoctorWarning
			facts = append(facts, "older than the gateway")
		}
		line.Detail = strings.Join(facts, ", ")
		if line.Status != DoctorOK {
			line.Hint = "control doctor " + n.Name
		}
		checks = append(checks, line)
	}
	return checks
}

func (d *doctorRun) machineMissing(c DoctorCheck) (DoctorCheck, bool) {
	switch {
	case !d.hasLogin():
		c.Status, c.Detail = DoctorSkipped, "no gateway login"
	case d.authErr != nil:
		c.Status, c.Detail = DoctorSkipped, "the gateway check did not complete"
	case d.directoryErr != nil:
		c.Status, c.Detail = DoctorSkipped, "the fleet directory is unavailable"
	case d.machine == nil:
		c.Status, c.Detail = DoctorSkipped, "machine not found"
	default:
		return c, false
	}
	return c, true
}

func (d *doctorRun) machineCheck() DoctorCheck {
	c := DoctorCheck{Check: "machine"}
	if d.hasLogin() && d.authErr == nil {
		if d.directoryErr != nil {
			c.Status, c.Detail = DoctorFailed, "cannot read the fleet directory: "+doctorErr(d.directoryErr)
			return c
		}
		if d.machine == nil {
			c.Status, c.Detail = DoctorFailed, fmt.Sprintf("no machine named %q is registered in this fleet", d.opts.Node)
			c.Hint = "control machines --compact lists registered names and IDs"
			return c
		}
	}
	if c, missing := d.machineMissing(c); missing {
		return c
	}
	m := d.machine
	entry := d.statusEntry(m.ID)
	c.Status = DoctorOK
	facts := []string{fmt.Sprintf("registered as %s (%s)", m.Name, doctorShortID(m.ID))}
	if m.RunAs != "" {
		facts = append(facts, "runs as "+m.RunAs)
	}
	if m.HybridRole != "" {
		facts = append(facts, "Hybrid "+m.HybridRole+" context")
	}
	var hints []string
	switch {
	case entry != nil && entry.Status == "update":
		c.Status = DoctorWarning
		state := "managed update in progress"
		if entry.UpdateState != "" {
			state += " (" + entry.UpdateState + ")"
		}
		facts = append(facts, state)
		hints = append(hints, "wait for the update to finish (docs/updates.md#idle-reservations-and-restart-behavior)")
	case m.Online:
		facts = append(facts, "online, connected since "+doctorWhen(m.LastSeen))
	default:
		c.Status = DoctorFailed
		facts = append(facts, "offline, last seen "+doctorWhen(m.LastSeen))
		if entry != nil && entry.WakeVia != "" && d.owner() {
			hints = append(hints, "if it is asleep, control wake "+m.Name+" can send a Wake-on-LAN packet via "+entry.WakeVia)
		} else {
			hints = append(hints, "check the Control service on that machine (control service status there)")
		}
	}
	if m.Disabled {
		c.Status = doctorWorse(c.Status, DoctorWarning)
		facts = append(facts, "disabled: excluded from scheduling and new execution")
		if d.owner() {
			hints = append(hints, "control machines enable "+m.Name)
		} else {
			hints = append(hints, "a fleet owner can re-enable it")
		}
	}
	if m.ControlPending {
		c.Status = doctorWorse(c.Status, DoctorWarning)
		facts = append(facts, "policy change not yet acknowledged, so nodes.select skips it")
		hints = append(hints, "wait for the node to reconnect and acknowledge (docs/dashboard.md#availability-is-separate-from-observability)")
	}
	if entry != nil && entry.Leased {
		c.Status = doctorWorse(c.Status, DoctorWarning)
		facts = append(facts, "an exclusive lease is active until "+doctorWhen(entry.LeaseExpires))
		hints = append(hints, "work in that leased sequence must carry its leaseId (docs/protocol.md#shared-pool-leases)")
	}
	c.Detail = strings.Join(facts, "; ")
	c.Hint = strings.Join(hints, "; ")
	return c
}

func doctorShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func doctorWorse(a, b string) string {
	rank := map[string]int{DoctorSkipped: 0, DoctorOK: 1, DoctorWarning: 2, DoctorFailed: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func (d *doctorRun) machineVersionCheck() DoctorCheck {
	c := DoctorCheck{Check: "machine-version"}
	if c, missing := d.machineMissing(c); missing {
		return c
	}
	m := d.machine
	arch := m.Software.Arch
	if m.System != nil && m.System.Arch != "" {
		arch = m.System.Arch
	}
	worker, gateway, release := m.Software.Version, d.gatewayVersion(), d.selectedRelease()
	c.Status = DoctorOK
	c.Detail = "worker " + doctorVersion(worker) + " (" + doctorPlatform(m.OS, arch) + ")"
	if gateway != "" {
		c.Detail += ", gateway " + gateway
	}
	c.Detail += ", client " + doctorVersion(d.software.Version)
	var notes []string
	if order, ok := doctorCompare(worker, gateway); ok && order < 0 {
		c.Status = DoctorWarning
		notes = append(notes, "the worker is older than the gateway")
	}
	if order, ok := doctorCompare(worker, release); ok && order < 0 {
		c.Status = DoctorWarning
		notes = append(notes, "the worker has not reached the fleet's selected release "+release)
	}
	if epoch := d.auth.GatewayProtocol; m.Software.NodeGatewayMax > 0 && !m.Software.SupportsGateway(epoch) {
		c.Status = DoctorFailed
		notes = append(notes, fmt.Sprintf("the worker supports gateway protocol %d..%d, not the gateway's %d", m.Software.NodeGatewayMin, m.Software.NodeGatewayMax, epoch))
	}
	if !m.InstructionDelegation {
		c.Status = DoctorFailed
		notes = append(notes, "the worker predates instruction-bound delegation, so account clients refuse to send it requests")
	}
	if len(notes) > 0 {
		c.Detail += "; " + strings.Join(notes, "; ")
		c.Hint = d.workerUpgradeHint()
	}
	return c
}

func (d *doctorRun) peerSkipReason() string {
	m := d.machine
	switch {
	case !m.Online:
		return "the machine is offline"
	case !m.InstructionDelegation:
		return "the worker must be upgraded before account clients send it requests"
	}
	return ""
}

func (d *doctorRun) peerSessionCheck() DoctorCheck {
	c := DoctorCheck{Check: "peer-session"}
	if c, missing := d.machineMissing(c); missing {
		return c
	}
	m := d.machine
	if reason := d.peerSkipReason(); reason != "" {
		c.Status, c.Detail = DoctorSkipped, reason
		return c
	}
	switch {
	case doctorNotPermitted(d.describeErr):
		c.Status, c.Detail = DoctorSkipped, "not permitted: "+doctorErr(d.describeErr)
		return c
	case errors.Is(d.describeErr, errDoctorNoDelegation):
		c.Status, c.Detail, c.Hint = DoctorFailed, doctorErr(d.describeErr), d.workerUpgradeHint()
		return c
	case d.describeErr != nil:
		c.Status = DoctorFailed
		c.Detail = "could not open a peer session to " + m.Name + ": " + doctorErr(d.describeErr)
		c.Hint = "the worker must hold a current gateway connection; control dashboard --once --node " + m.Name + " shows its state, and the transport falls back to the gateway relay by itself (docs/streaming.md#connection-layout)"
		return c
	case d.sessionErr != nil:
		c.Status, c.Detail = DoctorWarning, "a peer call succeeded, but this client's session list is unavailable: "+doctorErr(d.sessionErr)
		return c
	}
	var lanes []string
	relay, other := 0, 0
	for _, s := range d.session.Sessions {
		if s.Peer != m.ID {
			continue
		}
		kind := s.Mode
		if s.Route != nil && s.Route.Kind != "" {
			kind = s.Route.Kind
		}
		if kind == "gateway-relay" || s.Mode == "relay" {
			relay++
			kind = "gateway relay"
		} else {
			other++
			if s.Mode != "" && s.Mode != kind {
				kind = s.Mode + " " + kind
			}
		}
		lane := s.Lane
		if lane == "" {
			lane = "session"
		}
		lanes = append(lanes, lane+": "+kind)
	}
	slices.Sort(lanes)
	lanes = slices.Compact(lanes)
	c.Status = DoctorOK
	if len(lanes) == 0 {
		c.Detail = "a peer call to " + m.Name + " succeeded; no retained session was reported"
		return c
	}
	c.Detail = "connected to " + m.Name + " (" + strings.Join(lanes, ", ") + ")"
	if relay > 0 && other == 0 && !d.opts.RelayOnly {
		c.Status = DoctorWarning
		c.Detail += "; WebRTC did not connect, so traffic uses the gateway relay"
		c.Hint = "relay throughput is bounded by the gateway; control speedtest " + m.Name + " measures this path but sends test data and authorizes one connection.open grant (docs/connections.md)"
	} else if relay > 0 && d.opts.RelayOnly {
		c.Detail += "; this client is configured relay-only"
	}
	return c
}

func (d *doctorRun) concurrencyCheck() DoctorCheck {
	c := DoctorCheck{Check: "concurrency"}
	if c, missing := d.machineMissing(c); missing {
		return c
	}
	if reason := d.peerSkipReason(); reason != "" {
		c.Status, c.Detail = DoctorSkipped, reason
		return c
	}
	if d.describeErr != nil {
		c.Status, c.Detail = DoctorSkipped, "no peer session"
		return c
	}
	pools := d.describe.Concurrency
	if pools == nil {
		c.Status, c.Detail = DoctorSkipped, "the worker does not report concurrency; upgrade it for pool readings"
		return c
	}
	c.Status = DoctorOK
	var parts, busy []string
	for _, p := range []struct {
		name string
		pool *model.ConcurrencyPool
	}{
		{"execution", &pools.Execution}, {"operations", &pools.Operations}, {"desktop", pools.Desktop},
		{"input staging", pools.InputStaging}, {"output publication", pools.OutputPublication},
	} {
		if p.pool == nil {
			continue
		}
		part := fmt.Sprintf("%s %d/%d", p.name, p.pool.InUse, p.pool.Limit)
		if p.pool.Queued != nil && *p.pool.Queued > 0 {
			part += fmt.Sprintf(" (%d queued)", *p.pool.Queued)
		}
		parts = append(parts, part)
		if p.pool.Queued != nil && *p.pool.Queued > 0 || p.pool.Limit > 0 && p.pool.InUse >= p.pool.Limit {
			busy = append(busy, p.name)
		}
	}
	c.Detail = strings.Join(parts, ", ")
	if len(busy) > 0 {
		c.Status = DoctorWarning
		c.Detail += "; saturated: " + strings.Join(busy, ", ")
		c.Hint = "new work waits for a free slot; this is load, not a fault (docs/protocol.md#worker-concurrency)"
	}
	return c
}

func (d *doctorRun) diagnosticsCheck() DoctorCheck {
	c := DoctorCheck{Check: "diagnostics"}
	if c, missing := d.machineMissing(c); missing {
		return c
	}
	m := d.machine
	if reason := d.peerSkipReason(); reason != "" {
		c.Status, c.Detail = DoctorSkipped, reason
		return c
	}
	if m.InstructionOnly {
		c.Status = DoctorSkipped
		c.Detail = "instruction-only destination (such as a Hybrid System companion): diagnostics.get needs an exact grant, which doctor never requests"
		c.Hint = "control diagnostics " + m.Name + " authorizes that one read explicitly"
		return c
	}
	switch {
	case d.describeErr != nil:
		c.Status, c.Detail = DoctorSkipped, "no peer session"
		return c
	case doctorNotPermitted(d.diagErr):
		c.Status, c.Detail = DoctorSkipped, "not permitted: "+doctorErr(d.diagErr)
		if !ExecutionRole(d.role()) {
			c.Detail += "; diagnostics.get requires execution authority from an account or orchestrator key"
		}
		return c
	case d.diagErr != nil:
		c.Status, c.Detail = DoctorFailed, "diagnostics.get failed: "+doctorErr(d.diagErr)
		return c
	}
	g := d.diagnostics
	c.Status = DoctorOK
	facts := []string{fmt.Sprintf("%d active tasks", g.Tasks.Active)}
	var hints []string
	if g.Tasks.CleanupPending > 0 {
		c.Status = DoctorWarning
		facts = append(facts, fmt.Sprintf("%d task workspaces await cleanup", g.Tasks.CleanupPending))
		hints = append(hints, "cleanup retries at startup and every ten minutes (docs/protocol.md#node-configuration)")
	}
	if g.Artifacts.MaxBytes > 0 {
		percent := g.Artifacts.UsedBytes * 100 / g.Artifacts.MaxBytes
		facts = append(facts, fmt.Sprintf("artifacts %d%% of maxArtifactBytes", percent))
		if percent >= 90 {
			c.Status = DoctorWarning
			hints = append(hints, "delete unneeded artifacts or raise maxArtifactBytes (docs/protocol.md#node-configuration)")
		}
	} else {
		facts = append(facts, fmt.Sprintf("artifacts %d bytes, unlimited", g.Artifacts.UsedBytes))
	}
	facts = append(facts, fmt.Sprintf("MCP %d sessions, %d starting", g.MCP.Sessions, g.MCP.Starting))
	t := g.Transport
	facts = append(facts, fmt.Sprintf("since node start: %d gateway reconnects, %d gateway connect failures, %d relay fallbacks, %d link overflows", t.GatewayReconnects, t.GatewayConnectFailures, t.RelayFallbacks, t.LinkOverflows))
	if t.LinkOverflows > 0 {
		c.Status = DoctorWarning
		hints = append(hints, "a link overflow closes a link whose receiver falls behind its bounded queue (docs/streaming.md#limits-and-cleanup)")
	}
	c.Detail = strings.Join(facts, "; ")
	c.Hint = strings.Join(hints, "; ")
	return c
}
