package tunnel

import (
	"hash/fnv"
)

// Every QUIC DATAGRAM frame this tunnel sends carries a 1-byte type
// prefix so the TUN/L3 path and the legacy SOCKS5 UDP-ASSOCIATE relay
// path can share the same pool of QUIC connections without ambiguity.
//
// This is a wire-format break from pre-refactor QUICochet, where the
// UDP relay's [AssocID:4]... payload started at datagram byte 0 with
// no type prefix — client and server must be upgraded together.
const (
	// datagramTypeTUN marks a datagram whose remaining bytes are one
	// whole IP packet (v4 or v6), read verbatim off a TUN device and
	// to be written verbatim to the peer's TUN device. No further
	// framing: no assoc ID, no address, no length prefix — the IP
	// header itself carries everything a receiver needs.
	datagramTypeTUN byte = 0x00

	// datagramTypeUDPRelay marks a datagram using the legacy SOCKS5
	// UDP-ASSOCIATE relay format: [AssocID:4][ATYP+ADDR+PORT][DATA],
	// unchanged from pre-refactor QUICochet except for shifting one
	// byte to make room for this type prefix.
	datagramTypeUDPRelay byte = 0x01
)

// tunFlowHash derives a stable hash of an IP packet's inner flow
// (source/dest address + transport protocol + ports when present) so
// that every packet belonging to the same inner TCP/UDP flow lands on
// the same QUIC pool connection. This is required correctness, not
// just an optimisation: spreading one inner flow's packets across
// multiple independent QUIC connections would let them arrive
// out of order relative to each other (each connection has its own
// congestion control and loss recovery), which the inner TCP stack
// would read as packet loss/reordering and respond to by collapsing
// its congestion window.
//
// Returns 0 for anything that isn't a well-formed IPv4/IPv6 header —
// callers should treat that as "route on connection 0", never as an
// error; a truncated or malformed inner packet is still forwarded,
// just without the flow-affinity guarantee.
func tunFlowHash(pkt []byte) uint32 {
	if len(pkt) < 1 {
		return 0
	}
	version := pkt[0] >> 4
	switch version {
	case 4:
		return tunFlowHashV4(pkt)
	case 6:
		return tunFlowHashV6(pkt)
	default:
		return 0
	}
}

func tunFlowHashV4(pkt []byte) uint32 {
	const minIPv4Header = 20
	if len(pkt) < minIPv4Header {
		return 0
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < minIPv4Header || len(pkt) < ihl {
		ihl = minIPv4Header
	}
	proto := pkt[9]
	srcIP := pkt[12:16]
	dstIP := pkt[16:20]

	h := fnv.New32a()
	h.Write(srcIP)
	h.Write(dstIP)
	h.Write([]byte{proto})
	writeTransportPorts(h, pkt, ihl, proto)
	return h.Sum32()
}

func tunFlowHashV6(pkt []byte) uint32 {
	const ipv6HeaderLen = 40
	if len(pkt) < ipv6HeaderLen {
		return 0
	}
	nextHeader := pkt[6]
	srcIP := pkt[8:24]
	dstIP := pkt[24:40]

	h := fnv.New32a()
	h.Write(srcIP)
	h.Write(dstIP)
	h.Write([]byte{nextHeader})
	// IPv6 extension headers are not walked here — the common case
	// (TCP/UDP directly after the fixed header, no extension headers)
	// gets full 5-tuple hashing; anything else still gets a stable
	// per-(src,dst,next-header) hash, which is still flow-affine for
	// non-fragmented, non-extension-header traffic.
	writeTransportPorts(h, pkt, ipv6HeaderLen, nextHeader)
	return h.Sum32()
}

// writeTransportPorts feeds the source/destination ports into h when
// proto is TCP (6) or UDP (17) and the packet is long enough to
// contain them at offset. No-op otherwise (ICMP, fragments, GRE,
// etc. — those still get address+protocol hashing above).
func writeTransportPorts(h interface{ Write([]byte) (int, error) }, pkt []byte, offset int, proto byte) {
	if proto != 6 && proto != 17 { // TCP, UDP
		return
	}
	if len(pkt) < offset+4 {
		return
	}
	var portBuf [4]byte
	copy(portBuf[:], pkt[offset:offset+4])
	h.Write(portBuf[:])
}

// tunDestIPv4 extracts the destination IPv4 address from a raw IP
// packet, or the zero value + false if the packet isn't a
// well-formed IPv4 packet.
func tunDestIPv4(pkt []byte) (addr [4]byte, ok bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return addr, false
	}
	copy(addr[:], pkt[16:20])
	return addr, true
}

// tunDestIPv6 extracts the destination IPv6 address from a raw IP
// packet, or the zero value + false if the packet isn't a
// well-formed IPv6 packet.
func tunDestIPv6(pkt []byte) (addr [16]byte, ok bool) {
	if len(pkt) < 40 || pkt[0]>>4 != 6 {
		return addr, false
	}
	copy(addr[:], pkt[24:40])
	return addr, true
}
