// Package tun implements a minimal, dependency-free Linux TUN device:
// open /dev/net/tun, attach via TUNSETIFF, and configure address/MTU/
// flags via netlink-free ioctl calls on a throwaway AF_INET socket.
//
// The device is opened once at process startup and its lifetime is
// deliberately decoupled from the QUIC session pool: a spoof-IP
// rotation or a dropped/reconnected QUIC connection must never tear
// down or recreate the TUN interface, or every route/iptables rule an
// operator has layered on top of it would need reprogramming on every
// reconnect. See Device.Close — it is only ever called from the
// top-level Stop/shutdown path, never from the reconnect loop.
package tun

import (
	"fmt"
	"net"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ifnameSize = unix.IFNAMSIZ

	// TUNSETIFF/TUNSETPERSIST come from golang.org/x/sys/unix rather
	// than being hardcoded, because the ioctl request number is NOT
	// the same on every Linux architecture: the generic _IOC encoding
	// puts the "write" direction bit at 0x40000000 (amd64, arm64, arm,
	// 386, riscv64 → 0x400454ca), while the MIPS/PowerPC/SPARC
	// encoding puts it at 0x80000000 (mips*, ppc* → 0x800454ca). A
	// hardcoded generic value still compiles for those targets and
	// then fails at runtime with EINVAL on the very first TUNSETIFF,
	// so let x/sys supply the per-GOARCH value.
	//
	// The IFF_* values below ARE architecture-independent (they are
	// struct-field bit flags, not ioctl numbers), but come from the
	// same place for consistency.
	tunSetIff     = unix.TUNSETIFF
	tunSetPersist = unix.TUNSETPERSIST
	iffTun        = unix.IFF_TUN
	iffNoPI       = unix.IFF_NO_PI
	iffMultiQueue = unix.IFF_MULTI_QUEUE
)

type ifReq struct {
	Name  [ifnameSize]byte
	Flags uint16
	_     [22]byte // pad to sizeof(struct ifreq)
}

// Config describes how to create and configure a TUN device.
type Config struct {
	// Name is the interface name (e.g. "qc0"). If a device with this
	// name already exists and is a TUN device owned by this process
	// family, it is reused (supports TUNSETPERSIST across restarts).
	Name string

	// Local is this side's address on the point-to-point link, in
	// CIDR form (e.g. "10.20.0.2/24"). Required.
	Local string

	// Peer is the address of the far side of the point-to-point link
	// (e.g. "10.20.0.1"). When set, a host route to Peer is NOT added
	// automatically — the operator/manager script owns routing policy
	// on top of the interface; Device only brings the link up with
	// Local assigned.
	Peer string

	// MTU is the interface MTU. Must leave headroom for the QUIC
	// datagram ceiling (see initialPacketSize in internal/tunnel);
	// a TUN MTU larger than what the QUIC datagram path can carry
	// causes silent drops of oversized inner packets.
	MTU int

	// Persist, when true, sets TUNSETPERSIST so the interface and any
	// routes/iptables rules an operator has attached to it survive
	// this process exiting (e.g. across a supervised restart).
	Persist bool
}

// Device is an open, configured TUN interface. Reads return whole IP
// packets (IFF_NO_PI — no 4-byte protocol-family header prefix);
// writes must be whole IP packets, one per Write call.
//
// Deliberately does not wrap the fd in *os.File: Go's netpoller
// cannot register /dev/net/tun's character-device fd via epoll on
// stock kernels ("not pollable"), so Read/Write go straight through
// raw blocking syscalls instead — the same approach wireguard-go and
// other Go TUN implementations use. A blocked Read is only ever
// unblocked by Close (see the package doc comment on device lifetime).
type Device struct {
	fd   int
	name string
	mtu  int

	closeOnce sync.Once
	closeErr  error
}

// Open creates (or attaches to, if persistent and already present)
// the TUN device described by cfg, assigns its address, sets its MTU,
// and brings it up.
//
// Equivalent to OpenQueues(cfg, 1)[0] — a single, non-multiqueue
// device. Use OpenQueues directly when multiple parallel reader/
// writer goroutines are wanted (see OpenQueues's doc comment).
func Open(cfg Config) (*Device, error) {
	devs, err := OpenQueues(cfg, 1)
	if err != nil {
		return nil, err
	}
	return devs[0], nil
}

// OpenQueues creates (or attaches to, if persistent and already
// present) a multiqueue TUN device with n independent queues, each
// its own kernel-scheduled fd (IFF_MULTI_QUEUE — RSS-style hashing
// spreads packets across queues by inner flow, same mechanism a
// multiqueue physical NIC uses). n=1 opens a plain, non-multiqueue
// device (setting IFF_MULTI_QUEUE for a single queue is harmless but
// unnecessary).
//
// This is the concurrency answer to "batched I/O" for a TUN device:
// unlike a UDP socket, /dev/net/tun has no recvmmsg/sendmmsg
// equivalent — a single fd only ever transfers one packet per
// syscall. Multiple queues let N reader/writer goroutines pull from
// the kernel in parallel instead of serializing through one fd,
// which is what actually saturates a multi-core box under load.
// Pair with a matching number of core-pinned worker goroutines (see
// config.PerformanceConfig / tier presets).
//
// Every returned Device shares the same interface name, address, and
// MTU (configured once, from the first queue) but has an
// independently closable fd; callers should treat the returned slice
// as one logical device split across n readers, and Close() every
// entry on shutdown.
func OpenQueues(cfg Config, n int) ([]*Device, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("tun: name is required")
	}
	if len(cfg.Name) >= ifnameSize {
		return nil, fmt.Errorf("tun: interface name %q too long (max %d bytes)", cfg.Name, ifnameSize-1)
	}
	if cfg.Local == "" {
		return nil, fmt.Errorf("tun: local address is required")
	}
	if n < 1 {
		n = 1
	}
	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = 1360
	}

	flags := uint16(iffTun | iffNoPI)
	if n > 1 {
		flags |= iffMultiQueue
	}

	devs := make([]*Device, 0, n)
	closeAll := func() {
		for _, d := range devs {
			unix.Close(d.fd)
		}
	}

	for i := 0; i < n; i++ {
		fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("tun: open /dev/net/tun: %w", err)
		}

		var req ifReq
		copy(req.Name[:], cfg.Name)
		req.Flags = flags

		if err := ioctl(uintptr(fd), tunSetIff, uintptr(unsafe.Pointer(&req))); err != nil {
			unix.Close(fd)
			closeAll()
			return nil, fmt.Errorf("tun: TUNSETIFF %s (queue %d/%d): %w", cfg.Name, i+1, n, err)
		}

		if cfg.Persist {
			if err := ioctl(uintptr(fd), tunSetPersist, 1); err != nil {
				unix.Close(fd)
				closeAll()
				return nil, fmt.Errorf("tun: TUNSETPERSIST %s: %w", cfg.Name, err)
			}
		}

		devs = append(devs, &Device{fd: fd, name: cfg.Name, mtu: mtu})
	}

	// Address/MTU/up is an interface-level property, not per-queue —
	// configuring it once after all queues are attached is sufficient
	// and avoids n redundant ioctl round-trips.
	if err := configureAddr(cfg.Name, cfg.Local, mtu); err != nil {
		closeAll()
		return nil, err
	}

	return devs, nil
}

// Name returns the interface name.
func (d *Device) Name() string { return d.name }

// MTU returns the configured MTU.
func (d *Device) MTU() int { return d.mtu }

// Read reads one IP packet into p. p must be large enough for the
// device MTU; a short buffer truncates the packet (matches the
// standard TUN read semantics). Blocks until a packet arrives or the
// device is Closed (which returns EBADF here).
func (d *Device) Read(p []byte) (int, error) {
	n, err := unix.Read(d.fd, p)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Write writes one whole IP packet.
func (d *Device) Write(p []byte) (int, error) {
	n, err := unix.Write(d.fd, p)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Close closes the underlying file descriptor, which unblocks any
// goroutine parked in Read. When the device was opened with
// Persist=true, the kernel keeps the interface alive (routes and
// addresses stay configured) until a subsequent TUNSETPERSIST-off or
// reboot; a non-persistent device disappears immediately. Idempotent.
//
// Callers must only invoke this from the top-level shutdown path —
// see the package doc comment.
func (d *Device) Close() error {
	d.closeOnce.Do(func() {
		d.closeErr = unix.Close(d.fd)
	})
	return d.closeErr
}

// Fd exposes the raw file descriptor for callers that need direct
// access (e.g. batched I/O via readv/writev in a future optimisation).
func (d *Device) Fd() int { return d.fd }

func ioctl(fd uintptr, req uint, arg uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(req), arg)
	if errno != 0 {
		return errno
	}
	return nil
}

// configureAddr assigns the CIDR address, sets MTU, and brings the
// interface up using ioctls on a throwaway AF_INET socket (no
// external `ip` binary dependency).
func configureAddr(name, cidr string, mtu int) error {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("tun: parse local address %q: %w", cidr, err)
	}

	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("tun: create control socket: %w", err)
	}
	defer unix.Close(sock)

	if err := setIfAddr(sock, name, ip.To4()); err != nil {
		return fmt.Errorf("tun: set address: %w", err)
	}
	if err := setIfNetmask(sock, name, net.IP(ipNet.Mask)); err != nil {
		return fmt.Errorf("tun: set netmask: %w", err)
	}
	if err := setIfMTU(sock, name, mtu); err != nil {
		return fmt.Errorf("tun: set mtu: %w", err)
	}
	if err := setIfUp(sock, name); err != nil {
		return fmt.Errorf("tun: bring interface up: %w", err)
	}
	return nil
}

type ifReqAddr struct {
	Name [ifnameSize]byte
	Addr unix.RawSockaddrInet4
	_    [8]byte
}

func setIfAddr(sock int, name string, ip net.IP) error {
	var req ifReqAddr
	copy(req.Name[:], name)
	req.Addr.Family = unix.AF_INET
	copy(req.Addr.Addr[:], ip.To4())
	return ioctl(uintptr(sock), unix.SIOCSIFADDR, uintptr(unsafe.Pointer(&req)))
}

func setIfNetmask(sock int, name string, mask net.IP) error {
	var req ifReqAddr
	copy(req.Name[:], name)
	req.Addr.Family = unix.AF_INET
	copy(req.Addr.Addr[:], mask.To4())
	return ioctl(uintptr(sock), unix.SIOCSIFNETMASK, uintptr(unsafe.Pointer(&req)))
}

type ifReqMTU struct {
	Name [ifnameSize]byte
	MTU  int32
	_    [4]byte
}

func setIfMTU(sock int, name string, mtu int) error {
	var req ifReqMTU
	copy(req.Name[:], name)
	req.MTU = int32(mtu)
	return ioctl(uintptr(sock), unix.SIOCSIFMTU, uintptr(unsafe.Pointer(&req)))
}

func setIfUp(sock int, name string) error {
	var flagsReq ifReq
	copy(flagsReq.Name[:], name)
	if err := ioctl(uintptr(sock), unix.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&flagsReq))); err != nil {
		return err
	}
	flagsReq.Flags |= unix.IFF_UP | unix.IFF_RUNNING
	return ioctl(uintptr(sock), unix.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&flagsReq)))
}
