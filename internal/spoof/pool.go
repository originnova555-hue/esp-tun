// Package spoof keeps the list of source addresses the tunnel may send from,
// decides which one is live, and takes a bad one out of rotation.
//
// The addresses here are whitelisted by the censor's own filter, which is the
// whole point of the tunnel: traffic sourced from one of them survives a
// layer-3 blackout. They are also the fragile part — an address can start
// being dropped at any time, with no error reported to us — so the pool
// watches for silence and rotates away from an address that has gone quiet.
package spoof

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Activity is the signal the health loop watches. The session layer supplies
// it: there is no way to probe a spoofed source directly, so "is the peer
// still answering while we are talking" is the honest liveness test.
type Activity interface {
	// Counters returns cumulative packets sent and received on the transport.
	Counters() (tx, rx uint64)
	// LastRx is when a packet last arrived from the peer.
	LastRx() time.Time
}

// Options configures the health loop.
type Options struct {
	Enabled       bool
	Interval      time.Duration
	Timeout       time.Duration
	FailThreshold int
	Quarantine    time.Duration
	Logger        *slog.Logger
	// OnRotate runs after the active source changes. The session layer uses it
	// to tear down and redial, since the old sessions are talking to an
	// address the peer has stopped hearing.
	OnRotate func(from, to netip.Addr)
}

type source struct {
	addr netip.Addr

	mu               sync.Mutex
	fails            int
	quarantinedUntil time.Time
	lastGood         time.Time
	rotations        int
}

// Status is a point-in-time view of one source, for the admin socket and the
// metrics exporter.
type Status struct {
	Addr        netip.Addr `json:"addr"`
	Active      bool       `json:"active"`
	Quarantined bool       `json:"quarantined"`
	FailStreak  int        `json:"fail_streak"`
	LastGood    time.Time  `json:"last_good,omitzero"`
	Rotations   int        `json:"rotations"`
}

// Pool holds the candidate sources and the currently active one.
type Pool struct {
	opt     Options
	log     *slog.Logger
	sources []*source

	// active is read on every single packet, so it is an atomic pointer
	// rather than anything guarded by a mutex.
	active atomic.Pointer[netip.Addr]
	idx    atomic.Int32

	rotations atomic.Uint64
}

// NewPool builds a pool over the configured addresses. An empty list is
// allowed and means "do not spoof": Current then reports false and the
// transport sends with the kernel's own choice of source.
func NewPool(addrs []netip.Addr, opt Options) (*Pool, error) {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.FailThreshold <= 0 {
		opt.FailThreshold = 3
	}
	if opt.Interval <= 0 {
		opt.Interval = 15 * time.Second
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 5 * time.Second
	}
	if opt.Quarantine <= 0 {
		opt.Quarantine = 2 * time.Minute
	}
	p := &Pool{opt: opt, log: opt.Logger}
	seen := make(map[netip.Addr]bool, len(addrs))
	for _, a := range addrs {
		if !a.IsValid() {
			return nil, fmt.Errorf("spoof: invalid address %q", a)
		}
		a = a.Unmap()
		if seen[a] {
			continue
		}
		seen[a] = true
		p.sources = append(p.sources, &source{addr: a})
	}
	if len(p.sources) > 0 {
		p.setActive(0)
	}
	return p, nil
}

func (p *Pool) setActive(i int) {
	p.idx.Store(int32(i))
	a := p.sources[i].addr
	p.active.Store(&a)
}

// Current returns the address to send from. The second result is false when
// no spoofing is configured.
func (p *Pool) Current() (netip.Addr, bool) {
	a := p.active.Load()
	if a == nil {
		return netip.Addr{}, false
	}
	return *a, true
}

// Len is the number of configured sources.
func (p *Pool) Len() int { return len(p.sources) }

// Rotations counts how many times the pool has moved off a source.
func (p *Pool) Rotations() uint64 { return p.rotations.Load() }

// Snapshot describes every source.
func (p *Pool) Snapshot() []Status {
	now := time.Now()
	cur := int(p.idx.Load())
	out := make([]Status, 0, len(p.sources))
	for i, s := range p.sources {
		s.mu.Lock()
		out = append(out, Status{
			Addr:        s.addr,
			Active:      i == cur,
			Quarantined: now.Before(s.quarantinedUntil),
			FailStreak:  s.fails,
			LastGood:    s.lastGood,
			Rotations:   s.rotations,
		})
		s.mu.Unlock()
	}
	return out
}

// MarkGood clears the failure streak on the active source. The transport calls
// this whenever a packet arrives from the peer.
func (p *Pool) MarkGood() {
	if len(p.sources) == 0 {
		return
	}
	s := p.sources[p.idx.Load()]
	s.mu.Lock()
	s.fails = 0
	s.lastGood = time.Now()
	s.mu.Unlock()
}

// ForceRotate moves off the current source on demand. The admin socket uses
// it to take a suspect address out of rotation without waiting for the health
// loop to notice.
func (p *Pool) ForceRotate(reason string) bool { return p.rotate(reason) }

// rotate moves to the next source that is not quarantined. If every source is
// quarantined it picks the one that recovers soonest — sending from a
// suspect address beats sending from none.
func (p *Pool) rotate(reason string) bool {
	if len(p.sources) < 2 {
		return false
	}
	now := time.Now()
	cur := int(p.idx.Load())
	from := p.sources[cur].addr

	// Walk the other sources only: the current one is what we are rotating
	// away from, so it must never be a candidate for itself.
	best, bestUntil := -1, time.Time{}
	for off := 1; off < len(p.sources); off++ {
		i := (cur + off) % len(p.sources)
		s := p.sources[i]
		s.mu.Lock()
		until := s.quarantinedUntil
		s.mu.Unlock()
		if now.After(until) {
			best = i
			break
		}
		if bestUntil.IsZero() || until.Before(bestUntil) {
			best, bestUntil = i, until
		}
	}
	if best < 0 || best == cur {
		return false
	}
	p.sources[best].mu.Lock()
	p.sources[best].fails = 0
	p.sources[best].rotations++
	p.sources[best].mu.Unlock()

	p.setActive(best)
	p.rotations.Add(1)
	to := p.sources[best].addr
	p.log.Warn("rotating spoof source", "from", from, "to", to, "reason", reason)
	if p.opt.OnRotate != nil {
		p.opt.OnRotate(from, to)
	}
	return true
}

// Run drives the health loop until ctx is cancelled. It is a no-op when health
// checking is disabled or fewer than two sources are configured, since there
// would be nothing to rotate to.
func (p *Pool) Run(ctx context.Context, act Activity) {
	if !p.opt.Enabled || len(p.sources) == 0 || act == nil {
		return
	}
	t := time.NewTicker(p.opt.Interval)
	defer t.Stop()

	lastTx, _ := act.Counters()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tx, _ := act.Counters()
		sent := tx - lastTx
		lastTx = tx

		// Silence only means something if we were actually talking. A tunnel
		// with no traffic on it is quiet for entirely ordinary reasons.
		if sent == 0 {
			continue
		}
		if time.Since(act.LastRx()) <= p.opt.Timeout {
			p.MarkGood()
			continue
		}

		cur := p.sources[p.idx.Load()]
		cur.mu.Lock()
		cur.fails++
		streak := cur.fails
		cur.mu.Unlock()

		p.log.Warn("no reply from peer on active spoof source",
			"source", cur.addr, "fail_streak", streak,
			"threshold", p.opt.FailThreshold, "sent_since_last_check", sent)

		if streak < p.opt.FailThreshold {
			continue
		}
		cur.mu.Lock()
		cur.quarantinedUntil = time.Now().Add(p.opt.Quarantine)
		cur.fails = 0
		cur.mu.Unlock()
		p.log.Warn("quarantining spoof source",
			"source", cur.addr, "for", p.opt.Quarantine)
		p.rotate("no reply from peer")
	}
}
