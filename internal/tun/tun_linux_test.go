//go:build linux

package tun

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T, opt Options) *Device {
	t.Helper()
	d, err := Open(opt)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
			t.Skipf("needs /dev/net/tun and CAP_NET_ADMIN: %v", err)
		}
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestInterfaceComesUpConfigured(t *testing.T) {
	d := openTest(t, Options{
		Name:   "qctest0",
		Local:  netip.MustParsePrefix("10.99.0.1/30"),
		Peer:   netip.MustParseAddr("10.99.0.2"),
		MTU:    1360,
		Queues: 1,
	})
	if d.Name() != "qctest0" {
		t.Errorf("name = %q, want qctest0", d.Name())
	}
	out := ipShow(t, "qctest0")
	for _, want := range []string{"10.99.0.1/30", "mtu 1360", "UP"} {
		if !strings.Contains(out, want) {
			t.Errorf("interface is missing %q:\n%s", want, out)
		}
	}
}

func TestMultiqueueOpensEveryQueue(t *testing.T) {
	d := openTest(t, Options{
		Name:   "qctest1",
		Local:  netip.MustParsePrefix("10.99.1.1/30"),
		MTU:    1400,
		Queues: 4,
	})
	if d.Queues() != 4 {
		t.Fatalf("queues = %d, want 4", d.Queues())
	}
	for i := range 4 {
		if d.Queue(i) == nil {
			t.Errorf("queue %d is nil", i)
		}
	}
}

// TestPacketsRoundTripThroughTheKernel sends a packet at the interface and
// reads it back out of the queue, which is the only thing the datapath needs
// the device to do.
func TestPacketsRoundTripThroughTheKernel(t *testing.T) {
	d := openTest(t, Options{
		Name:   "qctest2",
		Local:  netip.MustParsePrefix("10.99.2.1/30"),
		Peer:   netip.MustParseAddr("10.99.2.2"),
		MTU:    1400,
		Queues: 1,
	})

	// Anything addressed into the /30 leaves through this interface.
	go func() {
		c, err := net.Dial("udp", "10.99.2.2:9")
		if err != nil {
			return
		}
		defer c.Close()
		for range 20 {
			c.Write([]byte("probe"))
			time.Sleep(20 * time.Millisecond)
		}
	}()

	buf := make([]byte, 2048)
	_ = d.Queue(0).SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := d.Queue(0).Read(buf)
	if err != nil {
		t.Fatalf("read from the interface: %v", err)
	}
	if n < 20 {
		t.Fatalf("read %d bytes, too short for an IP packet", n)
	}
	if v := buf[0] >> 4; v != 4 {
		t.Errorf("IP version = %d, want 4", v)
	}
	// Destination address sits at bytes 16..20 of an IPv4 header.
	dst, _ := netip.AddrFromSlice(buf[16:20])
	if dst != netip.MustParseAddr("10.99.2.2") {
		t.Errorf("destination = %v, want 10.99.2.2", dst)
	}
}

func TestNameTooLongIsRejected(t *testing.T) {
	if _, err := Open(Options{Name: "this-name-is-far-too-long", MTU: 1400}); err == nil {
		t.Error("an over-long interface name must be rejected")
	}
}

func ipShow(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("ip", "addr", "show", "dev", name).CombinedOutput()
	if err != nil {
		t.Skipf("iproute2 not available: %v", err)
	}
	return string(out)
}
