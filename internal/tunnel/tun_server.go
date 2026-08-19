package tunnel

import (
	"log/slog"

	"github.com/quic-go/quic-go"

	"github.com/pechenyeru/quiccochet/internal/affinity"
	"github.com/pechenyeru/quiccochet/internal/tun"
)

// registerTUNSession adds a live session to the per-peer set tunReadLoop
// picks from when routing a packet read off the shared TUN device.
// Mirrors registerReverseSession's shape/locking (see reverse_server.go).
func (s *Server) registerTUNSession(peer string, sess *quic.Conn) {
	s.tunPeerMu.Lock()
	defer s.tunPeerMu.Unlock()
	if s.tunPeerSessions == nil {
		s.tunPeerSessions = make(map[string]map[*quic.Conn]struct{})
	}
	set := s.tunPeerSessions[peer]
	if set == nil {
		set = make(map[*quic.Conn]struct{})
		s.tunPeerSessions[peer] = set
	}
	set[sess] = struct{}{}
}

// unregisterTUNSession removes a session on handleSession return.
func (s *Server) unregisterTUNSession(peer string, sess *quic.Conn) {
	s.tunPeerMu.Lock()
	defer s.tunPeerMu.Unlock()
	set := s.tunPeerSessions[peer]
	if set == nil {
		return
	}
	delete(set, sess)
	if len(set) == 0 {
		delete(s.tunPeerSessions, peer)
	}
}

// pickTUNSession returns a live QUIC session for the named peer,
// chosen by flowHash so every packet in the same inner flow lands on
// the same session (see tunFlowHash — required for inner TCP
// ordering, not just an optimisation). Returns (nil, false) when the
// peer has no live session.
func (s *Server) pickTUNSession(peer string, flowHash uint32) (*quic.Conn, bool) {
	s.tunPeerMu.Lock()
	set := s.tunPeerSessions[peer]
	if len(set) == 0 {
		s.tunPeerMu.Unlock()
		return nil, false
	}
	conns := make([]*quic.Conn, 0, len(set))
	for c := range set {
		conns = append(conns, c)
	}
	s.tunPeerMu.Unlock()

	n := uint32(len(conns))
	start := flowHash % n
	for i := uint32(0); i < n; i++ {
		c := conns[(start+i)%n]
		if c.Context().Err() == nil {
			return c, true
		}
	}
	return nil, false
}

// tunReadLoop reads whole IP packets off one TUN queue and routes
// each to the peer that owns its destination address (per
// peers[].tun_addr, resolved into s.tunRouteV4/V6 at NewServer time),
// sending it as a type-prefixed QUIC datagram on one of that peer's
// live sessions. A destination that matches no configured peer is
// silently dropped — the TUN subnet has no "outside" route, unlike a
// real router, because every address in it belongs to exactly one
// peer by construction (config.Validate enforces disjointness). One
// instance runs per queue in config.TUN.Queues (see Start).
//
// core >= 0 pins this goroutine's OS thread to that CPU core for the
// goroutine's entire lifetime (config.TUN.PinCores); core < 0 skips
// pinning entirely.
func (s *Server) tunReadLoop(dev *tun.Device, core int) {
	if core >= 0 {
		if err := affinity.PinCurrentGoroutine(core); err != nil {
			slog.Warn("tun: core pinning failed, continuing unpinned", "component", "tun", "core", core, "error", err)
		} else {
			slog.Debug("tun read loop: pinned", "component", "tun", "core", core)
		}
	}

	slog.Debug("tun read loop: start", "component", "tun")
	defer slog.Debug("tun read loop: exit", "component", "tun")

	mtu := dev.MTU()
	buf := make([]byte, 1+mtu+64) // +64 headroom for any inner header irregularity

	for s.running.Load() {
		n, err := dev.Read(buf[1:])
		if err != nil {
			if s.running.Load() {
				slog.Debug("tun: read failed", "component", "tun", "error", err)
			}
			return
		}
		if n == 0 {
			continue
		}
		inner := buf[1 : 1+n]

		peerName, ok := s.tunPeerForDest(inner)
		if !ok {
			continue
		}

		flowHash := tunFlowHash(inner)
		sess, ok := s.pickTUNSession(peerName, flowHash)
		if !ok {
			continue
		}

		buf[0] = datagramTypeTUN
		pkt := buf[:1+n]
		if err := sess.SendDatagram(pkt); err != nil {
			slog.Debug("tun: datagram send failed", "component", "tun", "peer", peerName, "size", n, "error", err)
			continue
		}

		s.bytesSent.Add(uint64(n))
		if pc := s.peerCountersByName(peerName); pc != nil {
			pc.bytesSent.Add(uint64(n))
			pc.touch()
		}
	}
}

// tunPeerForDest looks up which peer owns pkt's destination address
// in the TUN routing table.
func (s *Server) tunPeerForDest(pkt []byte) (string, bool) {
	if addr, ok := tunDestIPv4(pkt); ok {
		name, found := s.tunRouteV4[addr]
		return name, found
	}
	if addr, ok := tunDestIPv6(pkt); ok {
		name, found := s.tunRouteV6[addr]
		return name, found
	}
	return "", false
}

// handleTUNDatagram writes one inner IP packet received over QUIC
// from peerName to the shared server TUN device.
func (s *Server) handleTUNDatagram(pkt []byte, peerName string) {
	if len(s.tunDevs) == 0 || len(pkt) == 0 {
		return
	}
	dev := s.tunDevs[s.tunWriteIdx.Add(1)%uint32(len(s.tunDevs))]
	if _, err := dev.Write(pkt); err != nil {
		slog.Debug("tun: write failed", "component", "tun", "peer", peerName, "error", err)
		return
	}
	s.bytesReceived.Add(uint64(len(pkt)))
	if pc := s.peerCountersByName(peerName); pc != nil {
		pc.bytesReceived.Add(uint64(len(pkt)))
		pc.touch()
	}
}
