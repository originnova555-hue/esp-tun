//go:build linux

package transport

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// setTransparent enables IP_TRANSPARENT, without which the kernel refuses to
// send a packet whose source address is not one of its own. It needs
// CAP_NET_ADMIN. Both address families are attempted because a wildcard-bound
// socket can carry either; it is enough for one to succeed.
func setTransparent(rc syscall.RawConn) error {
	var err4, err6 error
	if cerr := rc.Control(func(fd uintptr) {
		err4 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TRANSPARENT, 1)
		err6 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_TRANSPARENT, 1)
	}); cerr != nil {
		return cerr
	}
	if err4 != nil && err6 != nil {
		return fmt.Errorf("IP_TRANSPARENT: %w (needs CAP_NET_ADMIN)", err4)
	}
	return nil
}

// BufferReport records what the socket buffers ended up being, and how.
type BufferReport struct {
	WantRcv, WantSnd int
	GotRcv, GotSnd   int
	ForcedRcv        bool
	ForcedSnd        bool
	Notes            []string
}

func (b BufferReport) String() string {
	f := func(forced bool) string {
		if forced {
			return " (forced)"
		}
		return ""
	}
	return fmt.Sprintf("rcv %s/%s%s, snd %s/%s%s",
		human(b.GotRcv), human(b.WantRcv), f(b.ForcedRcv),
		human(b.GotSnd), human(b.WantSnd), f(b.ForcedSnd))
}

func human(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%dK", n/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// setBuffers sizes the socket buffers, preferring the *FORCE variants so a low
// net.core.rmem_max does not silently cap us. The forced options need
// CAP_NET_ADMIN; without it we fall back to the ordinary options and report
// whatever the kernel was willing to give.
//
// This is worth the trouble: an undersized receive buffer shows up as bursty
// loss under load, which the layers above misread as congestion.
func setBuffers(rc syscall.RawConn, wantRcv, wantSnd int, force bool) (BufferReport, error) {
	rep := BufferReport{WantRcv: wantRcv, WantSnd: wantSnd}
	cerr := rc.Control(func(fd uintptr) {
		rep.ForcedRcv = setOneBuffer(int(fd), unix.SO_RCVBUF, unix.SO_RCVBUFFORCE, wantRcv, force)
		rep.ForcedSnd = setOneBuffer(int(fd), unix.SO_SNDBUF, unix.SO_SNDBUFFORCE, wantSnd, force)

		// The kernel reports back twice what it accounted for; halve it so the
		// number means the same thing as the one we asked for.
		if v, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF); err == nil {
			rep.GotRcv = v / 2
		}
		if v, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF); err == nil {
			rep.GotSnd = v / 2
		}
	})
	if cerr != nil {
		return rep, cerr
	}
	if rep.GotRcv*4 < wantRcv*3 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"receive buffer capped at %s of the %s requested; raise net.core.rmem_max "+
				"or run with CAP_NET_ADMIN", human(rep.GotRcv), human(wantRcv)))
	}
	if rep.GotSnd*4 < wantSnd*3 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"send buffer capped at %s of the %s requested; raise net.core.wmem_max "+
				"or run with CAP_NET_ADMIN", human(rep.GotSnd), human(wantSnd)))
	}
	return rep, nil
}

// setOneBuffer returns whether the forced option was the one that took effect.
func setOneBuffer(fd, opt, forceOpt, want int, force bool) bool {
	if force {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, forceOpt, want); err == nil {
			return true
		}
	}
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, opt, want)
	return false
}

// gsoSupported reports whether this kernel accepts UDP_SEGMENT, which is what
// lets one sendmsg carry several packets' worth of payload.
func gsoSupported(rc syscall.RawConn) bool {
	var ok bool
	_ = rc.Control(func(fd uintptr) {
		ok = unix.SetsockoptInt(int(fd), unix.IPPROTO_UDP, unix.UDP_SEGMENT, 0) == nil
	})
	return ok
}
