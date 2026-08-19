//go:build linux

package transport

import (
	"github.com/quic-go/quic-go"
	"golang.org/x/net/ipv4"
)

// The QUIC layer only keeps its optimised socket path — sendmmsg with
// segmentation offload out, recvmmsg in — for a connection that satisfies
// these two shapes. Asserting them here means a change to either side is a
// compile error rather than a silent fall back to one-packet-at-a-time I/O.
var _ quic.OOBCapablePacketConn = (*Conn)(nil)

var _ interface {
	ReadBatch(ms []ipv4.Message, flags int) (int, error)
} = (*Conn)(nil)
