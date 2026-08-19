package tunnel

import "testing"

// buildIPv4TCP builds a minimal (header-only) IPv4/TCP packet for
// hashing tests. Payload bytes beyond the TCP header are irrelevant
// to tunFlowHash.
func buildIPv4TCP(src, dst [4]byte, srcPort, dstPort uint16) []byte {
	pkt := make([]byte, 40) // 20 IPv4 + 20 TCP
	pkt[0] = 0x45           // version 4, IHL 5 (20 bytes)
	pkt[9] = 6              // TCP
	copy(pkt[12:16], src[:])
	copy(pkt[16:20], dst[:])
	pkt[20] = byte(srcPort >> 8)
	pkt[21] = byte(srcPort)
	pkt[22] = byte(dstPort >> 8)
	pkt[23] = byte(dstPort)
	return pkt
}

func buildIPv6TCP(src, dst [16]byte, srcPort, dstPort uint16) []byte {
	pkt := make([]byte, 60) // 40 IPv6 + 20 TCP
	pkt[0] = 0x60           // version 6
	pkt[6] = 6              // next header: TCP
	copy(pkt[8:24], src[:])
	copy(pkt[24:40], dst[:])
	pkt[40] = byte(srcPort >> 8)
	pkt[41] = byte(srcPort)
	pkt[42] = byte(dstPort >> 8)
	pkt[43] = byte(dstPort)
	return pkt
}

func TestTunFlowHashSameFlowIsStable(t *testing.T) {
	src := [4]byte{10, 20, 0, 2}
	dst := [4]byte{93, 184, 216, 34}
	pkt1 := buildIPv4TCP(src, dst, 51000, 443)
	pkt2 := buildIPv4TCP(src, dst, 51000, 443)

	h1 := tunFlowHash(pkt1)
	h2 := tunFlowHash(pkt2)
	if h1 != h2 {
		t.Fatalf("same 5-tuple hashed differently: %d vs %d", h1, h2)
	}
}

func TestTunFlowHashDifferentFlowsDiffer(t *testing.T) {
	src := [4]byte{10, 20, 0, 2}
	dst := [4]byte{93, 184, 216, 34}
	pktA := buildIPv4TCP(src, dst, 51000, 443)
	pktB := buildIPv4TCP(src, dst, 51001, 443) // different source port

	if tunFlowHash(pktA) == tunFlowHash(pktB) {
		t.Fatal("different 5-tuples produced the same hash (bad enough to break flow affinity in practice)")
	}
}

func TestTunFlowHashIPv6(t *testing.T) {
	src := [16]byte{0x20, 0x01, 0x0d, 0xb8}
	dst := [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	pkt1 := buildIPv6TCP(src, dst, 51000, 443)
	pkt2 := buildIPv6TCP(src, dst, 51000, 443)

	if tunFlowHash(pkt1) != tunFlowHash(pkt2) {
		t.Fatal("same IPv6 5-tuple hashed differently")
	}

	pkt3 := buildIPv6TCP(src, dst, 51001, 443)
	if tunFlowHash(pkt1) == tunFlowHash(pkt3) {
		t.Fatal("different IPv6 5-tuples produced the same hash")
	}
}

func TestTunFlowHashMalformedPacketReturnsZero(t *testing.T) {
	if h := tunFlowHash(nil); h != 0 {
		t.Errorf("expected 0 for nil packet, got %d", h)
	}
	if h := tunFlowHash([]byte{0x00}); h != 0 {
		t.Errorf("expected 0 for garbage version nibble, got %d", h)
	}
	// Truncated IPv4 header (less than 20 bytes) must not panic and
	// must return 0.
	if h := tunFlowHash([]byte{0x45, 0x00, 0x00}); h != 0 {
		t.Errorf("expected 0 for truncated IPv4 header, got %d", h)
	}
}

func TestTunFlowHashNonTCPUDPStillHashesAddresses(t *testing.T) {
	// ICMP (proto 1): no ports, but src/dst/proto must still produce a
	// consistent, non-zero-degenerate hash so ICMP packets stay
	// pinned to one connection too (matters for traceroute/MTU-probe
	// style bursts).
	pkt := make([]byte, 20)
	pkt[0] = 0x45
	pkt[9] = 1 // ICMP
	copy(pkt[12:16], []byte{10, 20, 0, 2})
	copy(pkt[16:20], []byte{8, 8, 8, 8})

	h1 := tunFlowHash(pkt)
	pkt2 := make([]byte, len(pkt))
	copy(pkt2, pkt)
	h2 := tunFlowHash(pkt2)
	if h1 != h2 {
		t.Fatal("identical ICMP packets hashed differently")
	}

	pkt3 := make([]byte, len(pkt))
	copy(pkt3, pkt)
	copy(pkt3[16:20], []byte{1, 1, 1, 1}) // different dest
	if tunFlowHash(pkt3) == h1 {
		t.Fatal("different ICMP destinations produced the same hash")
	}
}

func TestTunDestIPv4(t *testing.T) {
	pkt := buildIPv4TCP([4]byte{10, 20, 0, 2}, [4]byte{10, 20, 0, 5}, 1000, 2000)
	addr, ok := tunDestIPv4(pkt)
	if !ok {
		t.Fatal("expected ok=true for valid IPv4 packet")
	}
	want := [4]byte{10, 20, 0, 5}
	if addr != want {
		t.Errorf("addr = %v, want %v", addr, want)
	}

	if _, ok := tunDestIPv4([]byte{0x60, 0, 0}); ok {
		t.Error("expected ok=false for non-IPv4 / truncated packet")
	}
}

func TestTunDestIPv6(t *testing.T) {
	src := [16]byte{0x20, 0x01, 0x0d, 0xb8}
	dst := [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 9}
	pkt := buildIPv6TCP(src, dst, 1000, 2000)
	addr, ok := tunDestIPv6(pkt)
	if !ok {
		t.Fatal("expected ok=true for valid IPv6 packet")
	}
	if addr != dst {
		t.Errorf("addr = %v, want %v", addr, dst)
	}

	if _, ok := tunDestIPv6([]byte{0x45, 0, 0}); ok {
		t.Error("expected ok=false for non-IPv6 / truncated packet")
	}
}
