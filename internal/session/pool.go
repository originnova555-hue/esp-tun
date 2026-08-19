// Package session manages the QUIC connections that carry the tunnel.
//
// Payload rides in DATAGRAM frames (RFC 9221) and never in streams. That is
// the single most important decision in this package: a stream would make QUIC
// retransmit and re-order on behalf of traffic that is already doing its own
// recovery inside the tunnel, and stacking two reliability layers is a
// well-known way to turn a lossy path into a stalled one.
package session

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/originnova555-hue/esp-tun/internal/config"
	"github.com/originnova555-hue/esp-tun/internal/frame"
	"github.com/originnova555-hue/esp-tun/internal/transport"
)

// Options configures the pool.
type Options struct {
	Role      config.Role
	Conn      *transport.Conn
	Peer      netip.AddrPort // the address the client dials
	ServerTLS *tls.Config
	ClientTLS *tls.Config
	QUIC      config.QUIC
	Logger    *slog.Logger

	// OnDatagram is called for every datagram received, from the receiving
	// goroutine of the connection it arrived on. The slice is owned by the
	// callee; the QUIC layer has already copied it out of the packet buffer.
	OnDatagram func([]byte)
}

// Pool keeps a fixed number of connection slots to the peer.
//
// Traffic is spread across the slots by inner-flow hash rather than round
// robin. Round robin would maximise parallelism and also reorder every TCP
// flow inside the tunnel across N independent paths, which the receiving TCP
// reads as loss. Hashing keeps each inner flow on one connection — ordered —
// while still using every connection as soon as there is more than one flow,
// which is exactly the shape of the load this tunnel carries.
type Pool struct {
	opt   Options
	log   *slog.Logger
	tr    *quic.Transport
	qconf *quic.Config

	slots []*slot

	// maxDatagram is the largest payload the peer will accept in one
	// datagram. It starts optimistic and is corrected by the first
	// oversize probe, then tracks path MTU discovery downward.
	maxDatagram atomic.Int64

	sent, received   atomic.Uint64
	droppedFull      atomic.Uint64
	droppedNoLink    atomic.Uint64
	droppedTooLarge  atomic.Uint64
	handshakes       atomic.Uint64
	handshakeFailure atomic.Uint64
	lastActivityNano atomic.Int64

	generation atomic.Uint64 // bumped by Reset to retire the current links

	closeOnce sync.Once
	closed    chan struct{}
}

// slot is one position in the pool. Its link comes and goes; the slot does not.
type slot struct {
	index int
	link  atomic.Pointer[link]
}

// link is one live QUIC connection plus the goroutine feeding it.
type link struct {
	conn  *quic.Conn
	out   chan []byte
	since time.Time
	gen   uint64
}

// Stats is a point-in-time view for the admin socket and the metrics exporter.
type Stats struct {
	Slots            int       `json:"slots"`
	Live             int       `json:"live"`
	Sent             uint64    `json:"datagrams_sent"`
	Received         uint64    `json:"datagrams_received"`
	DroppedQueueFull uint64    `json:"dropped_queue_full"`
	DroppedNoLink    uint64    `json:"dropped_no_link"`
	DroppedTooLarge  uint64    `json:"dropped_too_large"`
	Handshakes       uint64    `json:"handshakes"`
	HandshakeFailed  uint64    `json:"handshake_failures"`
	MaxDatagram      int       `json:"max_datagram_bytes"`
	LastActivity     time.Time `json:"last_activity,omitzero"`
}

// New builds a pool. It does not connect; call Run for that.
func New(opt Options) (*Pool, error) {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Conn == nil {
		return nil, errors.New("session: no transport connection")
	}
	if opt.OnDatagram == nil {
		return nil, errors.New("session: no datagram handler")
	}
	if opt.Role == config.RoleClient && !opt.Peer.IsValid() {
		return nil, errors.New("session: client role needs a peer address")
	}
	n := opt.QUIC.PoolSize
	if n < 1 {
		n = 1
	}

	p := &Pool{
		opt:    opt,
		log:    opt.Logger,
		tr:     &quic.Transport{Conn: opt.Conn},
		closed: make(chan struct{}),
	}
	p.qconf = &quic.Config{
		EnableDatagrams:      true,
		MaxIdleTimeout:       opt.QUIC.MaxIdle(),
		HandshakeIdleTimeout: opt.QUIC.Handshake(),
		KeepAlivePeriod:      opt.QUIC.KeepAlive(),
		InitialPacketSize:    uint16(opt.QUIC.InitialPacketSize),
		// Streams are not part of the datapath. Refusing them keeps the peer
		// from opening any and makes that explicit on the wire.
		MaxIncomingStreams:      -1,
		MaxIncomingUniStreams:   -1,
		DisablePathMTUDiscovery: opt.QUIC.DisablePMTUD,
	}
	p.slots = make([]*slot, n)
	for i := range p.slots {
		p.slots[i] = &slot{index: i}
	}
	// Start optimistic; the probe on each new link corrects this downward the
	// moment the peer tells us it is too much.
	p.maxDatagram.Store(int64(opt.QUIC.InitialPacketSize - quicDatagramOverhead))
	return p, nil
}

// quicDatagramOverhead is a conservative allowance for the QUIC short header,
// packet number, DATAGRAM frame header and AEAD tag. Being wrong here is
// cheap: the first probe learns the real number from the peer.
const quicDatagramOverhead = 64

// MaxDatagram is the largest frame payload that will fit in one datagram.
func (p *Pool) MaxDatagram() int { return int(p.maxDatagram.Load()) }

// Run brings the pool up and keeps it up until ctx is cancelled.
func (p *Pool) Run(ctx context.Context) error {
	switch p.opt.Role {
	case config.RoleClient:
		return p.runClient(ctx)
	case config.RoleServer:
		return p.runServer(ctx)
	default:
		return fmt.Errorf("session: unknown role %q", p.opt.Role)
	}
}

// runClient gives every slot its own supervisor, so one slot reconnecting
// never stalls the others.
func (p *Pool) runClient(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, s := range p.slots {
		wg.Add(1)
		go func(s *slot) {
			defer wg.Done()
			p.superviseDial(ctx, s)
		}(s)
	}
	wg.Wait()
	return ctx.Err()
}

// superviseDial keeps one slot connected, backing off between attempts.
func (p *Pool) superviseDial(ctx context.Context, s *slot) {
	backoff := p.opt.QUIC.ReconnectMin()
	for {
		if ctx.Err() != nil {
			return
		}
		gen := p.generation.Load()
		conn, err := p.dial(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.handshakeFailure.Add(1)
			p.log.Warn("dial failed", "slot", s.index, "err", err, "retry_in", backoff)
			if !sleepCtx(ctx, jitter(backoff)) {
				return
			}
			backoff = nextBackoff(backoff, p.opt.QUIC.ReconnectMax())
			continue
		}
		backoff = p.opt.QUIC.ReconnectMin()
		p.handshakes.Add(1)
		p.log.Info("session up", "slot", s.index, "peer", p.opt.Peer,
			"cipher", tls.CipherSuiteName(conn.ConnectionState().TLS.CipherSuite))

		p.serve(ctx, s, conn, gen)

		if ctx.Err() != nil {
			return
		}
		// A connection that came up and then dropped gets one quick retry
		// before the backoff ladder starts again: most drops are transient.
		if !sleepCtx(ctx, jitter(p.opt.QUIC.ReconnectMin())) {
			return
		}
	}
}

func (p *Pool) dial(ctx context.Context) (*quic.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, p.opt.QUIC.Handshake()+2*time.Second)
	defer cancel()
	return p.tr.Dial(dctx, net.UDPAddrFromAddrPort(p.opt.Peer), p.opt.ClientTLS, p.qconf)
}

// runServer accepts connections and files each one into a slot.
func (p *Pool) runServer(ctx context.Context) error {
	ln, err := p.tr.Listen(p.opt.ServerTLS, p.qconf)
	if err != nil {
		return fmt.Errorf("session: listen: %w", err)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.handshakeFailure.Add(1)
			p.log.Warn("accept failed", "err", err)
			if !sleepCtx(ctx, 100*time.Millisecond) {
				return ctx.Err()
			}
			continue
		}
		p.handshakes.Add(1)
		s := p.claimSlot()
		p.log.Info("session up", "slot", s.index, "peer", conn.RemoteAddr(),
			"cipher", tls.CipherSuiteName(conn.ConnectionState().TLS.CipherSuite))
		gen := p.generation.Load()
		go p.serve(ctx, s, conn, gen)
	}
}

// claimSlot picks where an accepted connection belongs: an empty slot if there
// is one, otherwise the slot holding the oldest connection. Reusing the oldest
// matters during a client reconnect, when the replaced connection is still
// counting down its idle timeout and would otherwise hold a slot hostage.
func (p *Pool) claimSlot() *slot {
	var oldest *slot
	var oldestAt time.Time
	for _, s := range p.slots {
		l := s.link.Load()
		if l == nil {
			return s
		}
		if oldest == nil || l.since.Before(oldestAt) {
			oldest, oldestAt = s, l.since
		}
	}
	return oldest
}

// serve owns one connection for its whole life: publish it, pump datagrams
// both ways, and take it back out of the pool when it dies.
func (p *Pool) serve(ctx context.Context, s *slot, conn *quic.Conn, gen uint64) {
	l := &link{
		conn:  conn,
		out:   make(chan []byte, p.opt.QUIC.DatagramQueue),
		since: time.Now(),
		gen:   gen,
	}
	old := s.link.Swap(l)
	if old != nil {
		// Only close the connection; its sender exits when it notices the swap.
		_ = old.conn.CloseWithError(0, "replaced")
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.send(cctx, s, l) }()
	go func() { defer wg.Done(); p.receive(cctx, l) }()

	select {
	case <-conn.Context().Done():
	case <-cctx.Done():
	}
	cancel()
	_ = conn.CloseWithError(0, "")
	wg.Wait()

	s.link.CompareAndSwap(l, nil)
	if ctx.Err() == nil {
		p.log.Warn("session down", "slot", s.index, "up_for", time.Since(l.since).Truncate(time.Second),
			"reason", conn.Context().Err())
	}
}

// send is the only goroutine that calls SendDatagram on this connection.
func (p *Pool) send(ctx context.Context, s *slot, l *link) {
	p.probeDatagramSize(l)
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-l.out:
			if s.link.Load() != l {
				return // this link has been replaced
			}
			p.sendOne(l, b)
		}
	}
}

func (p *Pool) sendOne(l *link, b []byte) {
	err := l.conn.SendDatagram(b)
	if err == nil {
		p.sent.Add(1)
		p.lastActivityNano.Store(time.Now().UnixNano())
		return
	}
	var tooLarge *quic.DatagramTooLargeError
	if errors.As(err, &tooLarge) {
		p.droppedTooLarge.Add(1)
		p.shrinkDatagram(int(tooLarge.MaxDatagramPayloadSize))
		return
	}
	// Anything else means the connection is going away; serve will notice.
	p.log.Debug("send datagram failed", "err", err)
}

// probeDatagramSize learns the peer's datagram limit with one padding
// datagram, before any real traffic can be dropped for being too big.
func (p *Pool) probeDatagramSize(l *link) {
	for range 4 {
		want := int(p.maxDatagram.Load())
		if want < frame.HeaderSize {
			return
		}
		buf, err := frame.AppendPadding(make([]byte, 0, want), want)
		if err != nil {
			return
		}
		err = l.conn.SendDatagram(buf)
		if err == nil {
			return
		}
		var tooLarge *quic.DatagramTooLargeError
		if !errors.As(err, &tooLarge) {
			return
		}
		p.shrinkDatagram(int(tooLarge.MaxDatagramPayloadSize))
	}
}

// shrinkDatagram lowers the ceiling, never raises it: path MTU only shrinks
// within a connection's life, and a stale high value costs dropped packets.
func (p *Pool) shrinkDatagram(to int) {
	for {
		cur := p.maxDatagram.Load()
		if int64(to) >= cur {
			return
		}
		if p.maxDatagram.CompareAndSwap(cur, int64(to)) {
			p.log.Info("datagram ceiling lowered", "bytes", to)
			return
		}
	}
}

// receive pumps datagrams up into the datapath. It does as little as possible:
// the QUIC layer drops received datagrams once its own queue backs up, so time
// spent here turns directly into loss.
func (p *Pool) receive(ctx context.Context, l *link) {
	for {
		b, err := l.conn.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		p.received.Add(1)
		p.lastActivityNano.Store(time.Now().UnixNano())
		p.opt.OnDatagram(b)
	}
}

// Send queues a datagram on the connection this flow belongs to.
//
// A full queue drops rather than blocks. Blocking here would stall the reader
// pulling packets off the TUN interface, which turns one congested connection
// into a stall for every flow on the tunnel; dropping costs one packet that
// the inner flow's own recovery will replace.
func (p *Pool) Send(b []byte, flow uint32) error {
	n := uint32(len(p.slots))
	s := p.slots[flow%n]
	l := s.link.Load()
	if l == nil {
		// This slot is reconnecting. Rather than drop the packet, hand it to
		// any live slot: an out-of-order packet beats a lost one, and this
		// only happens while a connection is down.
		if l = p.anyLive(); l == nil {
			p.droppedNoLink.Add(1)
			return ErrNoSession
		}
	}
	select {
	case l.out <- b:
		return nil
	default:
		p.droppedFull.Add(1)
		return ErrQueueFull
	}
}

func (p *Pool) anyLive() *link {
	for _, s := range p.slots {
		if l := s.link.Load(); l != nil {
			return l
		}
	}
	return nil
}

// Errors returned by Send.
var (
	ErrNoSession = errors.New("session: no live connection")
	ErrQueueFull = errors.New("session: send queue full")
)

// Live counts the connected slots.
func (p *Pool) Live() int {
	n := 0
	for _, s := range p.slots {
		if s.link.Load() != nil {
			n++
		}
	}
	return n
}

// Ready reports whether at least one connection is up.
func (p *Pool) Ready() bool { return p.anyLive() != nil }

// LastActivity is when a datagram last moved in either direction.
func (p *Pool) LastActivity() time.Time {
	n := p.lastActivityNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Reset tears every connection down so they are redialled. The spoof pool
// calls this after rotating to a new source address: the existing connections
// are talking from an address the peer has stopped hearing, and no amount of
// waiting will bring them back.
func (p *Pool) Reset(reason string) {
	p.generation.Add(1)
	for _, s := range p.slots {
		if l := s.link.Load(); l != nil {
			_ = l.conn.CloseWithError(0, reason)
		}
	}
	p.log.Info("sessions reset", "reason", reason)
}

// Stats reports the pool's counters.
func (p *Pool) Stats() Stats {
	return Stats{
		Slots:            len(p.slots),
		Live:             p.Live(),
		Sent:             p.sent.Load(),
		Received:         p.received.Load(),
		DroppedQueueFull: p.droppedFull.Load(),
		DroppedNoLink:    p.droppedNoLink.Load(),
		DroppedTooLarge:  p.droppedTooLarge.Load(),
		Handshakes:       p.handshakes.Load(),
		HandshakeFailed:  p.handshakeFailure.Load(),
		MaxDatagram:      p.MaxDatagram(),
		LastActivity:     p.LastActivity(),
	}
}

// Close shuts the pool down.
func (p *Pool) Close() error {
	p.closeOnce.Do(func() {
		close(p.closed)
		for _, s := range p.slots {
			if l := s.link.Load(); l != nil {
				_ = l.conn.CloseWithError(0, "shutting down")
			}
		}
		_ = p.tr.Close()
	})
	return nil
}

// nextBackoff doubles up to a ceiling.
func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

// jitter spreads reconnect attempts so a pool of connections that dropped
// together does not come back as a synchronised burst.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d/2 + time.Duration(rand.Int64N(int64(d)))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
