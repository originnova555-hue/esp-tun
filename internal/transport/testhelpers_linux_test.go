//go:build linux

package transport

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// appendSegmentSize mirrors the segmentation-offload control message the QUIC
// layer appends, so the tests exercise the same buffer layout production does.
func appendSegmentSize(b []byte, size uint16) []byte {
	start := len(b)
	const dataLen = 2
	b = append(b, make([]byte, unix.CmsgSpace(dataLen))...)
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[start]))
	h.Level = unix.IPPROTO_UDP
	h.Type = unix.UDP_SEGMENT
	h.SetLen(unix.CmsgLen(dataLen))
	*(*uint16)(unsafe.Pointer(&b[start+unix.CmsgSpace(0)])) = size
	return b
}

func nativeUint16(b []byte) uint16 {
	if len(b) < 2 {
		return 0
	}
	return *(*uint16)(unsafe.Pointer(&b[0]))
}
