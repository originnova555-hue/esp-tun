//go:build linux

package transport

import (
	"net/netip"
	"testing"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// parseSrc pulls the source address back out of a control-message buffer, the
// same way the kernel does.
func parseSrc(t *testing.T, oob []byte, v6 bool) netip.Addr {
	t.Helper()
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		t.Fatalf("parse control messages: %v", err)
	}
	for _, m := range msgs {
		if !v6 && m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_PKTINFO {
			if len(m.Data) < sizeofInetPktinfo {
				t.Fatalf("in_pktinfo too short: %d", len(m.Data))
			}
			a, _ := netip.AddrFromSlice(m.Data[4:8])
			return a
		}
		if v6 && m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_PKTINFO {
			if len(m.Data) < sizeofInet6Pktinfo {
				t.Fatalf("in6_pktinfo too short: %d", len(m.Data))
			}
			a, _ := netip.AddrFromSlice(m.Data[0:16])
			return a
		}
	}
	return netip.Addr{}
}

func TestPatchPktinfoRewritesSourceInPlace(t *testing.T) {
	cm := ipv4.ControlMessage{Src: netip.MustParseAddr("192.0.2.1").AsSlice(), IfIndex: 7}
	oob := cm.Marshal()
	before := len(oob)

	want := netip.MustParseAddr("62.60.212.216")
	if !patchPktinfo(oob, want) {
		t.Fatal("patchPktinfo did not find the IP_PKTINFO message it was given")
	}
	if len(oob) != before {
		t.Errorf("buffer length changed from %d to %d; the patch must be in place", before, len(oob))
	}
	if got := parseSrc(t, oob, false); got != want {
		t.Errorf("source = %v, want %v", got, want)
	}

	// The interface index has to survive, so a multi-homed box still replies
	// out of the interface the request came in on.
	var got ipv4.ControlMessage
	if err := got.Parse(oob); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if got.IfIndex != 7 {
		t.Errorf("IfIndex = %d, want the original 7", got.IfIndex)
	}
}

func TestPatchPktinfoSkipsOtherControlMessages(t *testing.T) {
	// The QUIC layer appends its segmentation-offload message after the
	// packet-info one, so the scan has to walk past entries it does not own.
	cm := ipv4.ControlMessage{Src: netip.MustParseAddr("192.0.2.1").AsSlice()}
	oob := cm.Marshal()
	oob = appendSegmentSize(oob, 1200)

	want := netip.MustParseAddr("5.34.222.2")
	if !patchPktinfo(oob, want) {
		t.Fatal("patchPktinfo missed the packet-info message")
	}
	if got := parseSrc(t, oob, false); got != want {
		t.Errorf("source = %v, want %v", got, want)
	}
	// The offload message must be untouched.
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, m := range msgs {
		if m.Header.Level == unix.IPPROTO_UDP && m.Header.Type == unix.UDP_SEGMENT {
			found = true
			if sz := nativeUint16(m.Data); sz != 1200 {
				t.Errorf("segment size = %d, want 1200", sz)
			}
		}
	}
	if !found {
		t.Error("the UDP_SEGMENT message did not survive the patch")
	}
}

func TestPatchPktinfoReportsMissing(t *testing.T) {
	if patchPktinfo(nil, netip.MustParseAddr("192.0.2.1")) {
		t.Error("an empty buffer cannot contain a packet-info message")
	}
	oob := appendSegmentSize(nil, 1200)
	if patchPktinfo(oob, netip.MustParseAddr("192.0.2.1")) {
		t.Error("a buffer with only an offload message cannot be patched")
	}
}

func TestAppendPktinfoIPv4(t *testing.T) {
	want := netip.MustParseAddr("62.60.212.216")
	oob := appendPktinfo(nil, want)
	if got := parseSrc(t, oob, false); got != want {
		t.Errorf("source = %v, want %v", got, want)
	}
}

func TestAppendPktinfoIPv6(t *testing.T) {
	want := netip.MustParseAddr("2001:db8::1")
	oob := appendPktinfo(nil, want)
	if got := parseSrc(t, oob, true); got != want {
		t.Errorf("source = %v, want %v", got, want)
	}
}

func TestSetSourceOOBPatchesWithoutAllocating(t *testing.T) {
	cm := ipv4.ControlMessage{Src: netip.MustParseAddr("192.0.2.1").AsSlice()}
	oob := cm.Marshal()
	scratch := make([]byte, 0, 128)

	want := netip.MustParseAddr("198.51.100.7")
	out, gotScratch := setSourceOOB(oob, want, scratch)
	if &out[0] != &oob[0] {
		t.Error("the patched path must reuse the caller's buffer")
	}
	if cap(gotScratch) != cap(scratch) {
		t.Error("the patched path must not touch the scratch buffer")
	}
	if got := parseSrc(t, out, false); got != want {
		t.Errorf("source = %v, want %v", got, want)
	}
}

func TestSetSourceOOBAppendsWhenAbsent(t *testing.T) {
	want := netip.MustParseAddr("198.51.100.7")
	out, _ := setSourceOOB(nil, want, make([]byte, 0, 128))
	if got := parseSrc(t, out, false); got != want {
		t.Errorf("source = %v, want %v", got, want)
	}
}

func TestPatchIPv6PktinfoInPlace(t *testing.T) {
	cm := ipv6.ControlMessage{Src: netip.MustParseAddr("2001:db8::99").AsSlice(), IfIndex: 3}
	oob := cm.Marshal()
	want := netip.MustParseAddr("2001:db8::1")
	if !patchPktinfo(oob, want) {
		t.Fatal("patchPktinfo missed the IPv6 packet-info message")
	}
	if got := parseSrc(t, oob, true); got != want {
		t.Errorf("source = %v, want %v", got, want)
	}
}

func BenchmarkPatchPktinfo(b *testing.B) {
	cm := ipv4.ControlMessage{Src: netip.MustParseAddr("192.0.2.1").AsSlice()}
	oob := appendSegmentSize(cm.Marshal(), 1200)
	src := netip.MustParseAddr("62.60.212.216")
	b.ReportAllocs()
	for b.Loop() {
		if !patchPktinfo(oob, src) {
			b.Fatal("miss")
		}
	}
}
