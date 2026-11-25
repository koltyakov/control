package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

// Doctor check statuses. Only failed makes a report fail; warnings and
// skipped checks do not.
const (
	DoctorOK      = "ok"
	DoctorWarning = "warning"
	DoctorFailed  = "failed"
	DoctorSkipped = "skipped"
)

const (
	DefaultDoctorTimeout      = 30 * time.Second
	DefaultDoctorCheckTimeout = 10 * time.Second
	// Remote secret envelopes and gateway secret-management requests expire
	// 30 seconds after creation and are refused when they claim more than 60
	// seconds, so a 30-second offset breaks them outright. Warning at a third
	// of that budget leaves room for transit time and the worker's own offset.
	doctorSkewWarning = 10 * time.Second
	doctorSkewFailure = 30 * time.Second
	// doctorFleetLimit bounds the per-machine summary lines without a NODE.
	doctorFleetLimit  = 200
	doctorDetailLimit = 400
)

// DoctorCheck is one line of a doctor report. Hint is empty when there is
// nothing to do; every field is always present in JSON.
type DoctorCheck struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint"`
}

// DoctorSummary counts checks by status. Status is failed when any check
// failed, otherwise warning when any warned, otherwise ok.
type DoctorSummary struct {
	Status  string `json:"status"`
	OK      int    `json:"ok"`
	Warning int    `json:"warning"`
	Failed  int    `json:"failed"`
	Skipped int    `json:"skipped"`
}

// DoctorReport is the stable JSON shape of control doctor --json.
type DoctorReport struct {
	Node    string        `json:"node"`
	Checks  []DoctorCheck `json:"checks"`
	Summary DoctorSummary `json:"summary"`
}

// Failed reports whether any check failed.
func (r DoctorReport) Failed() bool { return r.Summary.Failed > 0 }

// WriteText prints the ordered human-readable report.
func (r DoctorReport) WriteText(w io.Writer) error {
	var b strings.Builder
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-8s %-18s %s\n", c.Status, c.Check, c.Detail)
		if c.Hint != "" {
			fmt.Fprintf(&b, "%-8s %-18s hint: %s\n", "", "", c.Hint)
		}
	}
	s := r.Summary
	fmt.Fprintf(&b, "\n%s: %d ok, %d warning, %d failed, %d skipped\n", s.Status, s.OK, s.Warning, s.Failed, s.Skipped)
	_, err := io.WriteString(w, b.String())
	return err
}

func (r *DoctorReport) add(checks ...DoctorCheck) {
	for _, c := range checks {
		c.Detail = doctorBound(c.Detail)
		r.Checks = append(r.Checks, c)
	}
}

func (r *DoctorReport) summarize() {
	s := DoctorSummary{}
	for _, c := range r.Checks {
		switch c.Status {
		case DoctorOK:
			s.OK++
		case DoctorWarning:
			s.Warning++
		case DoctorFailed:
			s.Failed++
		default:
			s.Skipped++
		}
	}
	s.Status = DoctorOK
	if s.Warning > 0 {
		s.Status = DoctorWarning
	}
	if s.Failed > 0 {
		s.Status = DoctorFailed
	}
	r.Summary = s
}

// DoctorOptions configures one read-only diagnosis. Credentials are used only
// for authentication and never appear in the report.
type DoctorOptions struct {
	// Node selects one machine by exact name or ID; empty summarizes the fleet
	// from gateway status without opening peer sessions.
	Node    string
	Gateway Admin
	// LoginSource names where Gateway.Key came from, never its value.
	LoginSource string
	// LocalAPI and LocalToken address this host's node API. LocalNode is the
	// configured node name, empty when this host has no node profile.
	LocalAPI, LocalToken, LocalNode string
	// RelayOnly reports that this client deliberately avoids WebRTC.
	RelayOnly             bool
	Timeout, CheckTimeout time.Duration
	// Getenv reads proxy variables; nil uses os.Getenv.
	Getenv func(string) string
	// Software replaces this binary's build information in tests.
	Software *buildinfo.Info
}

// doctorPeer is the only peer surface doctor uses. It is limited to the
// discovery and diagnostic reads below and cannot request grants.
type doctorPeer interface {
	observe(ctx context.Context, target, method string, params, result any) error
	session(ctx context.Context) (SessionInfo, error)
}

var (
	errDoctorInstructionOnly = errors.New("instruction-only destination")
	errDoctorNoDelegation    = errors.New("target does not support instruction-bound delegation; upgrade it before executing work")
)

type clientDoctorPeer struct{ c Client }

func (p clientDoctorPeer) observe(ctx context.Context, target, method string, params, result any) error {
	if method != "node.describe" && method != "diagnostics.get" {
		return fmt.Errorf("doctor does not call %s", method)
	}
	peer, err := p.c.backend(ctx)
	if err != nil {
		return err
	}
	if peer == nil {
		// Explicit local API authority: the local node forwards the read.
		return p.c.call(ctx, target, method, params, result)
	}
	destination, err := peer.Lookup(ctx, target)
	if err != nil {
		return err
	}
	if p.c.routing.cfg.AccountRouting && !destination.InstructionDelegation {
		return errDoctorNoDelegation
	}
	// Open the stream directly so no path can authorize an instruction-only
	// destination implicitly; only discovery reaches those.
	if destination.InstructionOnly && !instructionDiscovery(method) {
		return errDoctorInstructionOnly
	}
	conn, data, _, err := peer.OpenRPCWithReceipt(ctx, destination.ID, method, params)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return json.Unmarshal(data, result)
}

func (p clientDoctorPeer) session(ctx context.Context) (SessionInfo, error) { return p.c.Session(ctx) }

// Doctor runs bounded, read-only checks of this client, its gateway and,
// when opts.Node is set, one machine. It starts no task, requests no grant
// or lease, writes no worker file, sends no wake packet and retries nothing.
func (c Client) Doctor(ctx context.Context, opts DoctorOptions) DoctorReport {
	return runDoctor(ctx, opts, clientDoctorPeer{c})
}

type doctorAuth struct {
	Role            string `json:"role"`
	UserID          string `json:"userId"`
	Username        string `json:"username"`
	Delegation      bool   `json:"delegation"`
	ClientSessions  bool   `json:"clientSessions"`
	GatewayProtocol int    `json:"gatewayProtocol"`
}

type doctorStatus struct {
	pool           model.PoolActivitySnapshot
	sent, received time.Time
	date           time.Time
}

type doctorDescribe struct {
	Concurrency *model.NodeConcurrency `json:"concurrency"`
}

type doctorDiagnostics struct {
	Tasks struct {
		Active         int `json:"active"`
		CleanupPending int `json:"cleanupPending"`
	} `json:"tasks"`
	Artifacts struct {
		UsedBytes    int64 `json:"usedBytes"`
		MaxBytes     int64 `json:"maxBytes"`
		ReserveBytes int64 `json:"reserveBytes"`
		Imports      int   `json:"imports"`
	} `json:"artifacts"`
	MCP struct {
		Sessions int `json:"sessions"`
		Starting int `json:"starting"`
	} `json:"mcp"`
	Transport model.TransportStats `json:"transport"`
}

type doctorRun struct {
	ctx      context.Context
	opts     DoctorOptions
	peer     doctorPeer
	software buildinfo.Info

	auth         doctorAuth
	authErr      error
	status       doctorStatus
	statusErr    error
	selection    update.Selection
	selectionErr error
	directory    []model.Node
	directoryErr error
	localErr     error

	machine     *model.Node
	describe    doctorDescribe
	describeErr error
	session     SessionInfo
	sessionErr  error
	diagnostics doctorDiagnostics
	diagErr     error
}

func runDoctor(ctx context.Context, opts DoctorOptions, peer doctorPeer) DoctorReport {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultDoctorTimeout
	}
	if opts.CheckTimeout <= 0 {
		opts.CheckTimeout = DefaultDoctorCheckTimeout
	}
	if opts.CheckTimeout > opts.Timeout {
		opts.CheckTimeout = opts.Timeout
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	d := &doctorRun{ctx: ctx, opts: opts, peer: peer, software: buildinfo.Current()}
	if opts.Software != nil {
		d.software = *opts.Software
	}
	d.gatewayPhase()
	report := DoctorReport{Node: opts.Node, Checks: []DoctorCheck{}}
	report.add(d.loginCheck(), d.gatewayCheck(), d.versionCheck(), d.clockCheck(), d.localCheck(), d.environmentCheck())
	if opts.Node == "" {
		report.add(d.fleetChecks()...)
	} else {
		d.resolveMachine()
		d.peerPhase()
		report.add(d.machineCheck(), d.machineVersionCheck(), d.peerSessionCheck(), d.concurrencyCheck(), d.diagnosticsCheck())
	}
	report.summarize()
	return report
}

// doctorTimeout identifies a check abandoned at its own or the overall deadline.
type doctorTimeout struct{ message string }

func (e doctorTimeout) Error() string { return e.message }

func (d *doctorRun) timeoutErr() error {
	switch {
	case errors.Is(d.ctx.Err(), context.Canceled):
		return doctorTimeout{"cancelled"}
	case d.ctx.Err() != nil:
		return doctorTimeout{"overall deadline of " + d.opts.Timeout.String() + " reached"}
	}
	return doctorTimeout{"timed out after " + d.opts.CheckTimeout.String()}
}

type doctorResult[T any] struct {
	value T
	err   error
}

// doctorProbe runs one read under the per-check deadline. It returns at the
// deadline even when fn ignores cancellation, so a hung check cannot hang the
// report; fn's late result is discarded.
func doctorProbe[T any](d *doctorRun, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(d.ctx, d.opts.CheckTimeout)
	defer cancel()
	done := make(chan doctorResult[T], 1)
	go func() {
		value, err := fn(ctx)
		done <- doctorResult[T]{value, err}
	}()
	select {
	case r := <-done:
		if r.err != nil && ctx.Err() != nil {
			return r.value, d.timeoutErr()
		}
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, d.timeoutErr()
	}
}

func (d *doctorRun) hasLogin() bool { return d.opts.Gateway.URL != "" && d.opts.Gateway.Key != "" }

func (d *doctorRun) gatewayPhase() {
	var wg sync.WaitGroup
	gw := d.opts.Gateway
	if d.hasLogin() {
		wg.Go(func() {
			d.auth, d.authErr = doctorProbe(d, func(ctx context.Context) (doctorAuth, error) {
				var auth doctorAuth
				_, _, err := doctorGet(ctx, gw, "/v1/auth", &auth)
				return auth, err
			})
		})
		wg.Go(func() {
			d.status, d.statusErr = doctorProbe(d, func(ctx context.Context) (doctorStatus, error) {
				var s doctorStatus
				s.sent = time.Now()
				header, received, err := doctorGet(ctx, gw, "/v1/status", &s.pool)
				s.received = received
				if header != nil {
					s.date, _ = http.ParseTime(header.Get("Date"))
				}
				return s, err
			})
		})
		wg.Go(func() {
			d.selection, d.selectionErr = doctorProbe(d, func(ctx context.Context) (update.Selection, error) {
				var s update.Selection
				_, _, err := doctorGet(ctx, gw, "/v1/updates", &s)
				return s, err
			})
		})
		if d.opts.Node != "" {
			wg.Go(func() {
				d.directory, d.directoryErr = doctorProbe(d, func(ctx context.Context) ([]model.Node, error) {
					var nodes []model.Node
					_, _, err := doctorGet(ctx, gw, "/v1/nodes", &nodes)
					return nodes, err
				})
			})
		}
	}
	wg.Go(func() {
		_, d.localErr = doctorProbe(d, func(ctx context.Context) (struct{}, error) {
			if d.opts.LocalAPI == "" {
				return struct{}{}, errDoctorNoLocalAPI
			}
			var description doctorDescribe
			local := Client{URL: d.opts.LocalAPI, Token: d.opts.LocalToken}
			return struct{}{}, local.call(ctx, "", "node.describe", map[string]any{"view": "concurrency"}, &description)
		})
	})
	wg.Wait()
}

var errDoctorNoLocalAPI = errors.New("no local node API address")

func (d *doctorRun) resolveMachine() {
	for i := range d.directory {
		if n := &d.directory[i]; n.ID == d.opts.Node || n.Name == d.opts.Node {
			d.machine = n
			return
		}
	}
}

func (d *doctorRun) peerPhase() {
	m := d.machine
	if m == nil || !m.Online || !m.InstructionDelegation {
		return
	}
	d.describe, d.describeErr = doctorProbe(d, func(ctx context.Context) (doctorDescribe, error) {
		var description doctorDescribe
		err := d.peer.observe(ctx, m.ID, "node.describe", map[string]any{"view": "concurrency"}, &description)
		return description, err
	})
	if d.describeErr != nil {
		return
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		d.session, d.sessionErr = doctorProbe(d, d.peer.session)
	})
	// Instruction-only destinations need an exact grant for diagnostics.get.
	// Doctor never requests one; the check explains how to target it.
	if !m.InstructionOnly {
		wg.Go(func() {
			d.diagnostics, d.diagErr = doctorProbe(d, func(ctx context.Context) (doctorDiagnostics, error) {
				var diagnostics doctorDiagnostics
				err := d.peer.observe(ctx, m.ID, "diagnostics.get", map[string]any{}, &diagnostics)
				return diagnostics, err
			})
		})
	}
	wg.Wait()
}

// doctorHTTPError keeps the status code of a gateway refusal.
type doctorHTTPError struct {
	code int
	text string
}

func (e *doctorHTTPError) Error() string {
	if e.text == "" {
		return fmt.Sprintf("gateway returned HTTP %d", e.code)
	}
	return fmt.Sprintf("gateway returned HTTP %d: %s", e.code, e.text)
}

// doctorGet sends one authenticated GET over the profile's gateway route and
// reports when the response headers arrived.
func doctorGet(ctx context.Context, gw Admin, path string, result any) (http.Header, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gw.URL, "/")+path, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+gw.Key)
	resp, err := gw.Route.Client(0, false).Do(req)
	received := time.Now()
	if err != nil {
		return nil, received, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return resp.Header, received, &doctorHTTPError{code: resp.StatusCode, text: strings.TrimSpace(string(text))}
	}
	return resp.Header, received, json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(result)
}

func doctorHTTPStatus(err error) int {
	var e *doctorHTTPError
	if errors.As(err, &e) {
		return e.code
	}
	return 0
}

func doctorTimedOut(err error) bool {
	var e doctorTimeout
	return errors.As(err, &e)
}

// doctorNotPermitted recognizes a worker's authorization refusal, which is
// reported as skipped rather than as a failure.
func doctorNotPermitted(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	for _, refusal := range []string{"not authorized for", "permit discovery only", "outside delegated instruction"} {
		if strings.Contains(text, refusal) {
			return true
		}
	}
	return false
}

// doctorBound keeps details to one bounded line.
func doctorBound(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > doctorDetailLimit {
		cut := doctorDetailLimit
		for cut > 0 && text[cut]&0xC0 == 0x80 {
			cut--
		}
		text = text[:cut] + "..."
	}
	return text
}

func doctorErr(err error) string { return doctorBound(err.Error()) }
