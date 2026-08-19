//go:build linux

package transport

import (
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The source address of an outgoing packet is set per packet with an
// IP_PKTINFO / IPV6_PKTINFO control message rather than by binding the socket
// or by building the IP header ourselves.
//
// This matters for throughput, not just for tidiness. A raw IP_HDRINCL socket
// — the obvious way to forge a source — takes the packet off the kernel's
// fast path entirely: no GSO, no checksum offload, one syscall and one
// software checksum per packet. Handing the kernel a normal UDP payload and
// overriding only the source in a cmsg keeps segmentation offload and the
// batched sendmmsg path intact, which is where the bulk of the throughput on
// this datapath comes from.
//
// A non-local source is rejected unless the socket carries IP_TRANSPARENT,
// which is why setTransparent runs before any of this.

// sizes of the kernel structs carried in the control message.
const (
	sizeofInetPktinfo  = 12 // struct in_pktinfo:  ifindex u32, spec_dst 4B, addr 4B
	sizeofInet6Pktinfo = 20 // struct in6_pktinfo: addr 16B, ifindex u32
)

// cmsgDataOffset is where a control message's payload starts, relative to the
// start of its header.
var cmsgDataOffset = unix.CmsgLen(0)

// patchPktinfo rewrites the source address of an existing PKTINFO control
// message in place and reports whether it found one. This is the common case:
// the socket is bound to a wildcard address, so the QUIC layer already emits a
// PKTINFO cmsg on every send and we only have to change four bytes in it —
// no allocation on the packet path.
func patchPktinfo(oob []byte, src netip.Addr) bool {
	for i := 0; i+unix.SizeofCmsghdr <= len(oob); {
		h := (*unix.Cmsghdr)(unsafe.Pointer(&oob[i]))
		l := int(h.Len)
		if l < unix.SizeofCmsghdr || i+l > len(oob) {
			return false // malformed; leave it alone
		}
		data := oob[i+cmsgDataOffset : i+l]
		switch {
		case src.Is4() && h.Level == unix.IPPROTO_IP && h.Type == unix.IP_PKTINFO &&
			len(data) >= sizeofInetPktinfo:
			b := src.As4()
			// ipi_spec_dst is what the kernel uses as the outgoing source.
			// ipi_ifindex is left as the QUIC layer set it, so a multi-homed
			// box still replies out of the interface the request arrived on.
			copy(data[4:8], b[:])
			return true
		case src.Is6() && h.Level == unix.IPPROTO_IPV6 && h.Type == unix.IPV6_PKTINFO &&
			len(data) >= sizeofInet6Pktinfo:
			b := src.As16()
			copy(data[0:16], b[:])
			return true
		}
		step := cmsgAlign(l)
		if step <= 0 {
			return false
		}
		i += step
	}
	return false
}

// appendPktinfo adds a PKTINFO control message carrying src. It is used on the
// first packets of a connection, before the QUIC layer has a local address to
// report, and whenever the socket is bound to a specific address.
func appendPktinfo(oob []byte, src netip.Addr) []byte {
	var (
		level, typ int32
		dataLen    int
	)
	if src.Is4() {
		level, typ, dataLen = unix.IPPROTO_IP, unix.IP_PKTINFO, sizeofInetPktinfo
	} else {
		level, typ, dataLen = unix.IPPROTO_IPV6, unix.IPV6_PKTINFO, sizeofInet6Pktinfo
	}
	start := len(oob)
	oob = append(oob, make([]byte, unix.CmsgSpace(dataLen))...)

	h := (*unix.Cmsghdr)(unsafe.Pointer(&oob[start]))
	h.Level = level
	h.Type = typ
	h.SetLen(unix.CmsgLen(dataLen))

	data := oob[start+cmsgDataOffset:]
	if src.Is4() {
		b := src.As4()
		copy(data[4:8], b[:]) // ipi_spec_dst; ifindex stays 0 so routing decides
	} else {
		b := src.As16()
		copy(data[0:16], b[:])
	}
	return oob
}

// setSourceOOB returns a control-message buffer that makes the kernel send
// from src, reusing the caller's buffer when it can. scratch is only touched
// when a new control message has to be built.
func setSourceOOB(oob []byte, src netip.Addr, scratch []byte) ([]byte, []byte) {
	if patchPktinfo(oob, src) {
		return oob, scratch
	}
	scratch = append(scratch[:0], oob...)
	scratch = appendPktinfo(scratch, src)
	return scratch, scratch
}

func cmsgAlign(n int) int {
	const align = int(unsafe.Sizeof(uintptr(0)))
	return (n + align - 1) &^ (align - 1)
}
