package gateway

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Limits bound live admission and retained packet buffers, not process RSS or
// durable fleet history. Zero option fields select defaults; negatives fail.
type Limits struct {
	Connections            int   `json:"connections"`
	ConnectionsPerAccount  int   `json:"connectionsPerAccount"`
	ClientsPerAccount      int   `json:"clientsPerAccount"`
	NodesPerAccount        int   `json:"nodesPerAccount"`
	Handshakes             int   `json:"handshakes"`
	HandshakesPerAccount   int   `json:"handshakesPerAccount"`
	NewIdentitiesPerMinute int   `json:"newIdentitiesPerMinute"`
	QueuedBytes            int64 `json:"queuedBytes"`
	QueuedBytesPerAccount  int64 `json:"queuedBytesPerAccount"`
}

func DefaultLimits() Limits {
	return Limits{Connections: 1024, ConnectionsPerAccount: 256, ClientsPerAccount: 64,
		NodesPerAccount: 192, Handshakes: 64, HandshakesPerAccount: 16,
		NewIdentitiesPerMinute: 120, QueuedBytes: 64 << 20, QueuedBytesPerAccount: 16 << 20}
}

// Validate checks explicit runtime settings, including flags. A zero-valued
// Options.Limits is separately defaulted by New for embedding callers.
func (l Limits) Validate() error {
	if l.Connections <= 0 || l.ConnectionsPerAccount <= 0 || l.ClientsPerAccount <= 0 || l.NodesPerAccount <= 0 ||
		l.Handshakes <= 0 || l.HandshakesPerAccount <= 0 || l.NewIdentitiesPerMinute <= 0 ||
		l.QueuedBytes <= 0 || l.QueuedBytesPerAccount <= 0 {
		return errors.New("gateway admission and packet byte limits must be positive")
	}
	return nil
}

func (l Limits) defaults() (Limits, error) {
	d := DefaultLimits()
	for _, pair := range [][2]*int{{&l.Connections, &d.Connections}, {&l.ConnectionsPerAccount, &d.ConnectionsPerAccount},
		{&l.ClientsPerAccount, &d.ClientsPerAccount}, {&l.NodesPerAccount, &d.NodesPerAccount},
		{&l.Handshakes, &d.Handshakes}, {&l.HandshakesPerAccount, &d.HandshakesPerAccount}, {&l.NewIdentitiesPerMinute, &d.NewIdentitiesPerMinute}} {
		if *pair[0] == 0 {
			*pair[0] = *pair[1]
		}
	}
	if l.QueuedBytes == 0 {
		l.QueuedBytes = d.QueuedBytes
	}
	if l.QueuedBytesPerAccount == 0 {
		l.QueuedBytesPerAccount = d.QueuedBytesPerAccount
	}
	return l, l.Validate()
}

type packetBudget struct {
	used  atomic.Int64
	peak  atomic.Int64
	limit int64
}

func (b *packetBudget) reserve(size int64, data bool) bool {
	limit := b.limit
	// Keep control headroom even under bulk load.
	if data {
		limit -= limit / 16
	}
	for {
		used := b.used.Load()
		if size > limit-used {
			return false
		}
		if b.used.CompareAndSwap(used, used+size) {
			for peak := b.peak.Load(); used+size > peak; peak = b.peak.Load() {
				if b.peak.CompareAndSwap(peak, used+size) {
					break
				}
			}
			return true
		}
	}
}

type accountAdmission struct {
	connections, clients, nodes, handshakes int
	tokens                                  float64
	refilled                                time.Time
	packets                                 packetBudget
}

type admission struct {
	mu                      sync.Mutex
	limits                  Limits
	connections, handshakes int
	accounts                map[string]*accountAdmission
	packets                 packetBudget
	rejected                atomic.Int64
	identityRejected        atomic.Int64
	queueRejected           atomic.Int64
	handshakeCount          atomic.Int64
	handshakeNanos          atomic.Int64
	writeFailures           atomic.Int64
}

func newAdmission(l Limits) *admission {
	return &admission{limits: l, accounts: make(map[string]*accountAdmission), packets: packetBudget{limit: l.QueuedBytes}}
}

type connectionReservation struct {
	admission                     *admission
	account                       *accountAdmission
	user                          string
	client, handshaking, released bool
	started                       time.Time
}

func (a *admission) reserve(user string, client bool) (*connectionReservation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.accounts[user]
	if s == nil {
		s = &accountAdmission{tokens: float64(a.limits.NewIdentitiesPerMinute), refilled: time.Now(), packets: packetBudget{limit: a.limits.QueuedBytesPerAccount}}
		a.accounts[user] = s
	}
	l := a.limits
	if a.connections >= l.Connections || s.connections >= l.ConnectionsPerAccount ||
		a.handshakes >= l.Handshakes || s.handshakes >= l.HandshakesPerAccount ||
		(client && s.clients >= l.ClientsPerAccount) || (!client && s.nodes >= l.NodesPerAccount) {
		a.rejected.Add(1)
		return nil, false
	}
	a.connections++
	a.handshakes++
	s.connections++
	s.handshakes++
	if client {
		s.clients++
	} else {
		s.nodes++
	}
	return &connectionReservation{admission: a, account: s, user: user, client: client, handshaking: true, started: time.Now()}, true
}

func (r *connectionReservation) finishHandshake() {
	r.admission.mu.Lock()
	defer r.admission.mu.Unlock()
	r.finishHandshakeLocked()
}

func (r *connectionReservation) finishHandshakeLocked() {
	if r.handshaking {
		r.handshaking = false
		r.admission.handshakes--
		r.account.handshakes--
		r.admission.handshakeCount.Add(1)
		r.admission.handshakeNanos.Add(time.Since(r.started).Nanoseconds())
	}
}

func (r *connectionReservation) release() {
	r.admission.mu.Lock()
	defer r.admission.mu.Unlock()
	if r.released {
		return
	}
	r.released = true
	r.finishHandshakeLocked()
	r.admission.connections--
	r.account.connections--
	if r.client {
		r.account.clients--
	} else {
		r.account.nodes--
	}
}

func (r *connectionReservation) allowNewIdentity() bool {
	a := r.admission
	a.mu.Lock()
	defer a.mu.Unlock()
	s := r.account
	now := time.Now()
	s.tokens = min(float64(a.limits.NewIdentitiesPerMinute), s.tokens+now.Sub(s.refilled).Seconds()*float64(a.limits.NewIdentitiesPerMinute)/60)
	s.refilled = now
	if s.tokens < 1 {
		a.identityRejected.Add(1)
		return false
	}
	s.tokens--
	return true
}
