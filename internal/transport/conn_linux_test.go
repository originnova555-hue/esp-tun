//go:build linux

package transport

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/originnova555-hue/esp-tun/internal/spoof"
)

func newPool(t *testing.T, addrs ...string) *spoof.Pool {
	t.Helper()
	var as []netip.Addr
	for _, s := range addrs {
		as = append(as, netip.MustParseAddr(s))
	}
	p, err := spoof.NewPool(as, spoof.Options{})
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	return p
}

func listenLoopback(t *testing.T, opt Options) *Conn {
	t.Helper()
	if opt.Listen == (netip.AddrPort{}) {
		opt.Listen = netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
	}
	if opt.Pool == nil {
		opt.Pool = newPool(t)
	}
	c, err := Listen(opt)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("needs CAP_NET_ADMIN: %v", err)
		}
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestSpoofedSourceReachesTheWire is the claim the whole transport rests on:
// a packet leaves with a source address that does not belong to this host, and
// the receiver sees that address rather than ours.
func TestSpoofedSourceReachesTheWire(t *testing.T) {
	const forged = "203.0.113.77"

	rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("receiver: %v", err)
	}
	defer rx.Close()

	tx := listenLoopback(t, Options{
		Pool:         newPool(t, forged),
		Transparent:  true,
		RcvBuffer:    1 << 20,
		SndBuffer:    1 << 20,
		BatchedReads: true,
	})

	dst := rx.LocalAddr().(*net.UDPAddr)
	if _, err := tx.WriteTo([]byte("hello"), dst); err != nil {
		t.Fatalf("send: %v", err)
	}

	buf := make([]byte, 64)
	_ = rx.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, from, err := rx.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Errorf("payload = %q, want %q", buf[:n], "hello")
	}
	if got := from.IP.String(); got != forged {
		t.Errorf("source address on the wire = %s, want the forged %s", got, forged)
	}
}

// TestSourceFollowsPoolRotation checks that changing the active source changes
// what goes out, without reopening the socket.
func TestSourceFollowsPoolRotation(t *testing.T) {
	rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("receiver: %v", err)
	}
	defer rx.Close()
	dst := rx.LocalAddr().(*net.UDPAddr)

	pool := newPool(t, "203.0.113.11", "203.0.113.12")
	tx := listenLoopback(t, Options{
		Pool: pool, Transparent: true, RcvBuffer: 1 << 20, SndBuffer: 1 << 20,
	})

	read := func() string {
		buf := make([]byte, 64)
		_ = rx.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, from, err := rx.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		return from.IP.String()
	}

	if _, err := tx.WriteTo([]byte("a"), dst); err != nil {
		t.Fatalf("send: %v", err)
	}
	first := read()

	// Drive a rotation the way the health loop would.
	pool.ForceRotate("test")

	if _, err := tx.WriteTo([]byte("b"), dst); err != nil {
		t.Fatalf("send: %v", err)
	}
	second := read()

	if first == second {
		t.Errorf("both packets came from %s; rotation did not change the source", first)
	}
	for _, got := range []string{first, second} {
		if got != "203.0.113.11" && got != "203.0.113.12" {
			t.Errorf("unexpected source %s", got)
		}
	}
}

// TestUnexpectedSourceIsFiltered checks that a packet from an address outside
// spoof_expect is blanked rather than handed up.
func TestUnexpectedSourceIsFiltered(t *testing.T) {
	rx := listenLoopback(t, Options{
		Listen:       netip.MustParseAddrPort("127.0.0.1:0"),
		Expect:       []netip.Addr{netip.MustParseAddr("203.0.113.99")},
		RcvBuffer:    1 << 20,
		SndBuffer:    1 << 20,
		BatchedReads: true,
	})
	dstPort := rx.LocalAddr().(*net.UDPAddr).Port

	tx, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: dstPort})
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	defer tx.Close()
	if _, err := tx.Write([]byte("from an address we did not ask for")); err != nil {
		t.Fatalf("send: %v", err)
	}

	ms := []ipv4.Message{{Buffers: [][]byte{make([]byte, 2048)}, OOB: make([]byte, 128)}}
	_ = rx.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := rx.ReadBatch(ms, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 1 {
		t.Fatalf("read %d messages, want 1", n)
	}
	if ms[0].N != 0 {
		t.Errorf("message length = %d, want 0 — an unexpected source must be blanked", ms[0].N)
	}
	if rx.Filtered() != 1 {
		t.Errorf("filtered counter = %d, want 1", rx.Filtered())
	}
	if _, rxp := rx.Counters(); rxp != 0 {
		t.Errorf("receive counter = %d, want 0 for a filtered packet", rxp)
	}
}

func TestExpectedSourcePassesAndCountsUp(t *testing.T) {
	rx := listenLoopback(t, Options{
		Listen:       netip.MustParseAddrPort("127.0.0.1:0"),
		Expect:       []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		RcvBuffer:    1 << 20,
		SndBuffer:    1 << 20,
		BatchedReads: true,
	})
	dstPort := rx.LocalAddr().(*net.UDPAddr).Port

	tx, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: dstPort})
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	defer tx.Close()
	if _, err := tx.Write([]byte("expected")); err != nil {
		t.Fatalf("send: %v", err)
	}

	ms := []ipv4.Message{{Buffers: [][]byte{make([]byte, 2048)}, OOB: make([]byte, 128)}}
	_ = rx.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := rx.ReadBatch(ms, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 1 || ms[0].N != len("expected") {
		t.Fatalf("read n=%d len=%d, want 1 message of %d bytes", n, ms[0].N, len("expected"))
	}
	if _, rxp := rx.Counters(); rxp != 1 {
		t.Errorf("receive counter = %d, want 1", rxp)
	}
	if rx.LastRx().IsZero() {
		t.Error("LastRx should have been stamped")
	}
}

func TestBuffersAreSizedAndReported(t *testing.T) {
	const want = 8 << 20
	c := listenLoopback(t, Options{
		RcvBuffer: want, SndBuffer: want, ForceBuffers: true,
	})
	b := c.Buffers()
	if b.GotRcv < want {
		t.Errorf("receive buffer = %d, want at least %d (report: %s)", b.GotRcv, want, b)
	}
	if b.GotSnd < want {
		t.Errorf("send buffer = %d, want at least %d (report: %s)", b.GotSnd, want, b)
	}
}
