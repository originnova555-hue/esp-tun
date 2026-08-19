//go:build linux

// Package tun owns the layer-3 interface the tunnel carries.
//
// The interface is created once, at startup, and stays up for the life of the
// process. It is deliberately not tied to the session: a QUIC connection can
// drop and redial a dozen times and the interface never moves, so routes,
// firewall rules and anything else the operator hung off it stay valid. A
// tunnel that recreates its interface on every reconnect makes every one of
// those a transient outage for reasons that have nothing to do with the link.
package tun

import (
	"fmt"
	"net"
	"net/netip"
	"os"

	"golang.org/x/sys/unix"
)

// Device is a TUN interface together with its queues.
type Device struct {
	name   string
	mtu    int
	queues []*os.File
}

// Options configures interface creation.
type Options struct {
	Name string
	// Local is the address given to the interface, with its prefix.
	Local netip.Prefix
	// Peer is the address at the other end. It is used for the route only.
	Peer netip.Addr
	MTU  int
	// Queues is how many file descriptors serve the interface. More than one
	// needs IFF_MULTI_QUEUE and lets several cores read from the interface at
	// the same time instead of contending on one queue.
	Queues int
	// Persist keeps the interface alive after the process exits. That is
	// usually not what you want for a tunnel, but it makes a restart seamless
	// for anything already routed through it.
	Persist bool
}

// Open creates the interface and brings it up.
func Open(opt Options) (*Device, error) {
	if opt.Queues < 1 {
		opt.Queues = 1
	}
	if len(opt.Name) == 0 || len(opt.Name) > unix.IFNAMSIZ-1 {
		return nil, fmt.Errorf("tun: interface name %q must be 1-%d characters",
			opt.Name, unix.IFNAMSIZ-1)
	}

	d := &Device{name: opt.Name, mtu: opt.MTU}
	flags := uint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if opt.Queues > 1 {
		flags |= unix.IFF_MULTI_QUEUE
	}

	for i := range opt.Queues {
		f, err := openQueue(opt.Name, flags)
		if err != nil {
			d.Close()
			if i > 0 {
				return nil, fmt.Errorf("tun: opening queue %d of %d: %w "+
					"(multiqueue needs a kernel with IFF_MULTI_QUEUE)", i+1, opt.Queues, err)
			}
			return nil, err
		}
		d.queues = append(d.queues, f)
	}

	if opt.Persist {
		if err := unix.IoctlSetInt(int(d.queues[0].Fd()), unix.TUNSETPERSIST, 1); err != nil {
			d.Close()
			return nil, fmt.Errorf("tun: set persist: %w", err)
		}
	}
	if err := d.configure(opt); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func openQueue(name string, flags uint16) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("tun: open /dev/net/tun: %w "+
			"(the module must be loaded and the process needs CAP_NET_ADMIN)", err)
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	ifr.SetUint16(flags)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("tun: create %q: %w", name, err)
	}
	// Handing the descriptor to os.File puts it under the runtime's poller,
	// so a blocked read parks the goroutine instead of an OS thread.
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun"), nil
}

// configure assigns the address, sets the MTU and brings the interface up,
// using ioctls rather than shelling out, so there is no dependency on
// iproute2 being installed.
func (d *Device) configure(opt Options) error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("tun: control socket: %w", err)
	}
	defer unix.Close(sock)

	set := func(req uint, build func(*unix.Ifreq) error) error {
		ifr, err := unix.NewIfreq(d.name)
		if err != nil {
			return err
		}
		if err := build(ifr); err != nil {
			return err
		}
		return unix.IoctlIfreq(sock, req, ifr)
	}

	if opt.Local.IsValid() {
		addr := opt.Local.Addr()
		if !addr.Is4() {
			return fmt.Errorf("tun: %s is not an IPv4 address; "+
				"the interface is configured over AF_INET ioctls", addr)
		}
		a := addr.As4()
		if err := set(unix.SIOCSIFADDR, func(ifr *unix.Ifreq) error {
			return ifr.SetInet4Addr(a[:])
		}); err != nil {
			return fmt.Errorf("tun: set address %s: %w", addr, err)
		}
		mask := net.CIDRMask(opt.Local.Bits(), 32)
		if err := set(unix.SIOCSIFNETMASK, func(ifr *unix.Ifreq) error {
			return ifr.SetInet4Addr(mask)
		}); err != nil {
			return fmt.Errorf("tun: set netmask /%d: %w", opt.Local.Bits(), err)
		}
	}
	if opt.MTU > 0 {
		if err := set(unix.SIOCSIFMTU, func(ifr *unix.Ifreq) error {
			ifr.SetUint32(uint32(opt.MTU))
			return nil
		}); err != nil {
			return fmt.Errorf("tun: set MTU %d: %w", opt.MTU, err)
		}
	}

	// Read the current flags before setting them, so nothing already on the
	// interface is cleared by the write.
	ifr, err := unix.NewIfreq(d.name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(sock, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("tun: read flags: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(sock, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("tun: bring %q up: %w", d.name, err)
	}
	return nil
}

// Name is the interface name.
func (d *Device) Name() string { return d.name }

// MTU is the configured interface MTU.
func (d *Device) MTU() int { return d.mtu }

// Queues is how many descriptors serve the interface.
func (d *Device) Queues() int { return len(d.queues) }

// Queue returns the descriptor for one queue. Each is meant to be driven by
// exactly one goroutine, which is what makes multiqueue worth having.
func (d *Device) Queue(i int) *os.File { return d.queues[i] }

// Close tears every queue down. The interface itself disappears with the last
// descriptor unless it was created with Persist.
func (d *Device) Close() error {
	var first error
	for _, f := range d.queues {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	d.queues = nil
	return first
}
