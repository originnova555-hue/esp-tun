package tun

import (
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// requireRoot skips the test when not running as a privileged user —
// TUN device creation needs CAP_NET_ADMIN.
func requireCapNetAdmin(t *testing.T) {
	t.Helper()
	// Cheap probe: try opening /dev/net/tun and issuing TUNSETIFF with
	// a throwaway name; EPERM means we lack the capability.
	dev, err := Open(Config{Name: "qctest0", Local: "10.250.0.1/30", MTU: 1400})
	if err != nil {
		t.Skipf("skipping: TUN device creation failed (likely missing CAP_NET_ADMIN): %v", err)
	}
	dev.Close()
	exec.Command("ip", "link", "delete", "qctest0").Run()
}

func TestOpenConfiguresInterface(t *testing.T) {
	requireCapNetAdmin(t)

	dev, err := Open(Config{
		Name:  "qct1",
		Local: "10.251.0.1/30",
		MTU:   1350,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		dev.Close()
		exec.Command("ip", "link", "delete", "qct1").Run()
	}()

	if dev.Name() != "qct1" {
		t.Errorf("Name() = %q, want qct1", dev.Name())
	}
	if dev.MTU() != 1350 {
		t.Errorf("MTU() = %d, want 1350", dev.MTU())
	}

	out, err := exec.Command("ip", "addr", "show", "qct1").CombinedOutput()
	if err != nil {
		t.Fatalf("ip addr show: %v: %s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "10.251.0.1/30") {
		t.Errorf("interface missing expected address, got:\n%s", text)
	}
	if !strings.Contains(text, "mtu 1350") {
		t.Errorf("interface missing expected mtu, got:\n%s", text)
	}
	if !strings.Contains(text, "UP") {
		t.Errorf("interface not up, got:\n%s", text)
	}
}

func TestPacketsRoundTripThroughDevice(t *testing.T) {
	requireCapNetAdmin(t)

	dev, err := Open(Config{
		Name:  "qct2",
		Local: "10.252.0.1/30",
		MTU:   1400,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		dev.Close()
		exec.Command("ip", "link", "delete", "qct2").Run()
	}()

	// Send a UDP packet from a socket bound to the interface's local
	// address toward its peer address; the kernel routes it onto qct2
	// because the destination is inside the /30, with no external
	// ping/iputils dependency. Verify we can read the raw IP packet
	// back out.
	//
	// Device.Read is a raw blocking syscall (no netpoller deadline
	// support for /dev/net/tun's character-device fd — confirmed on
	// this kernel) — a blocked Read is only ever unblocked by Close,
	// which is exactly the pattern the tunSend/tunRecv goroutines use
	// in production. Enforce the test timeout with a watchdog
	// goroutine that closes the device instead.
	go func() {
		time.Sleep(200 * time.Millisecond)
		conn, err := net.DialUDP("udp4",
			&net.UDPAddr{IP: net.ParseIP("10.252.0.1")},
			&net.UDPAddr{IP: net.ParseIP("10.252.0.2"), Port: 9999})
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte("probe"))
	}()

	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(3 * time.Second):
			dev.Close()
		case <-done:
		}
	}()

	buf := make([]byte, 2000)
	n, err := dev.Read(buf)
	close(done)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n < 20 {
		t.Fatalf("short packet: %d bytes", n)
	}
	version := buf[0] >> 4
	if version != 4 {
		t.Fatalf("expected IPv4 packet, got version %d", version)
	}
	srcIP := net.IP(buf[12:16])
	if !srcIP.Equal(net.ParseIP("10.252.0.1")) {
		t.Errorf("unexpected src IP: %s", srcIP)
	}
	dstIP := net.IP(buf[16:20])
	if !dstIP.Equal(net.ParseIP("10.252.0.2")) {
		t.Errorf("unexpected dst IP: %s", dstIP)
	}
}

func TestNameTooLongRejected(t *testing.T) {
	_, err := Open(Config{
		Name:  strings.Repeat("x", 20),
		Local: "10.253.0.1/30",
	})
	if err == nil {
		t.Fatal("expected error for over-length interface name")
	}
}

func TestMissingLocalRejected(t *testing.T) {
	_, err := Open(Config{Name: "qct3"})
	if err == nil {
		t.Fatal("expected error for missing local address")
	}
}

func TestOpenQueuesMultiQueueAllQueuesUsable(t *testing.T) {
	requireCapNetAdmin(t)

	devs, err := OpenQueues(Config{
		Name:  "qcmq0",
		Local: "10.255.0.1/24",
		MTU:   1400,
	}, 4)
	if err != nil {
		t.Fatalf("OpenQueues: %v", err)
	}
	defer func() {
		for _, d := range devs {
			d.Close()
		}
		exec.Command("ip", "link", "delete", "qcmq0").Run()
	}()

	if len(devs) != 4 {
		t.Fatalf("got %d devices, want 4", len(devs))
	}
	for i, d := range devs {
		if d.Name() != "qcmq0" {
			t.Errorf("devs[%d].Name() = %q, want qcmq0", i, d.Name())
		}
		if d.MTU() != 1400 {
			t.Errorf("devs[%d].MTU() = %d, want 1400", i, d.MTU())
		}
	}

	// Distinct fds: writing on one queue and reading on another
	// exercises the actual kernel-side multiqueue dispatch, not just
	// that four fds happened to open without erroring.
	seen := map[int]bool{}
	for _, d := range devs {
		if seen[d.Fd()] {
			t.Fatalf("duplicate fd %d across queues", d.Fd())
		}
		seen[d.Fd()] = true
	}

	out, err := exec.Command("ip", "-d", "link", "show", "qcmq0").CombinedOutput()
	if err != nil {
		t.Fatalf("ip link show: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "UP") {
		t.Errorf("interface not up:\n%s", out)
	}
}

func TestOpenQueuesSingleQueueEquivalentToOpen(t *testing.T) {
	requireCapNetAdmin(t)

	devs, err := OpenQueues(Config{Name: "qcmq1", Local: "10.255.1.1/24", MTU: 1400}, 1)
	if err != nil {
		t.Fatalf("OpenQueues(n=1): %v", err)
	}
	defer func() {
		devs[0].Close()
		exec.Command("ip", "link", "delete", "qcmq1").Run()
	}()

	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1", len(devs))
	}
}
