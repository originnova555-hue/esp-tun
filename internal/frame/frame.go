// Package frame defines what goes inside a QUIC datagram.
//
// One datagram carries one or more frames, which is what lets the datapath
// pack several small IP packets into a single datagram. That packing is the
// main answer to the workload this tunnel actually sees: hundreds of
// concurrent flows made of DNS queries, HTTP keep-alives, ACKs and WebRTC —
// packets far smaller than the path MTU. Sending each of those as its own
// datagram wastes most of every packet on headers and pays the per-datagram
// cost of the QUIC layer once per tiny payload.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Frame types.
const (
	// TypeIP carries one IP packet, verbatim.
	TypeIP byte = 0x01
	// TypePad carries nothing. It exists so a datagram can be grown to a
	// chosen size, and so a cover datagram has something legal to contain.
	TypePad byte = 0x02
	// TypeControl carries an out-of-band message between the two ends.
	TypeControl byte = 0x03
)

// HeaderSize is the per-frame overhead: one type byte and a two-byte length.
const HeaderSize = 3

// MaxPayload is the largest payload a single frame can describe.
const MaxPayload = 0xFFFF

// ErrTruncated means a datagram ended in the middle of a frame.
var ErrTruncated = errors.New("frame: truncated datagram")

// Append writes one frame to dst and returns the extended slice.
func Append(dst []byte, typ byte, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayload {
		return dst, fmt.Errorf("frame: payload of %d bytes exceeds the %d-byte limit",
			len(payload), MaxPayload)
	}
	dst = append(dst, typ, 0, 0)
	binary.BigEndian.PutUint16(dst[len(dst)-2:], uint16(len(payload)))
	return append(dst, payload...), nil
}

// AppendPadding writes a padding frame whose total on-the-wire size is exactly
// total bytes. total must be at least HeaderSize.
func AppendPadding(dst []byte, total int) ([]byte, error) {
	if total < HeaderSize {
		return dst, fmt.Errorf("frame: padding of %d bytes is smaller than the %d-byte header",
			total, HeaderSize)
	}
	n := total - HeaderSize
	dst = append(dst, TypePad, 0, 0)
	binary.BigEndian.PutUint16(dst[len(dst)-2:], uint16(n))
	// The padding bytes are never read, so their content does not matter for
	// correctness. Appending from the zero value keeps this allocation-free
	// when dst has the capacity, which it does on the packet path.
	for range n {
		dst = append(dst, 0)
	}
	return dst, nil
}

// Walk calls fn for each frame in a datagram. The payload handed to fn aliases
// the datagram, so fn must copy anything it intends to keep.
func Walk(datagram []byte, fn func(typ byte, payload []byte) error) error {
	for len(datagram) > 0 {
		if len(datagram) < HeaderSize {
			return ErrTruncated
		}
		typ := datagram[0]
		n := int(binary.BigEndian.Uint16(datagram[1:3]))
		if len(datagram) < HeaderSize+n {
			return ErrTruncated
		}
		payload := datagram[HeaderSize : HeaderSize+n]
		if typ != TypePad {
			if err := fn(typ, payload); err != nil {
				return err
			}
		}
		datagram = datagram[HeaderSize+n:]
	}
	return nil
}
