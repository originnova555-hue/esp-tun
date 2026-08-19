//go:build linux

// Package transport is the UDP socket underneath QUIC. It exists to do one
// thing the standard socket cannot: send every packet with a source address
// that is not ours, while keeping the kernel's batched, offloaded send and
// receive paths fully intact.
package transport

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/originnova555-hue/esp-tun/internal/spoof"
)

// Options configures the socket.
type Options struct {
	Listen       netip.AddrPort
	Pool         *spoof.Pool
	Expect       []netip.Addr // accepted peer sources; empty means accept any
	RcvBuffer    int
	SndBuffer    int
	ForceBuffers bool
	Transparent  bool
	GSO          bool
	BatchedReads bool
	Logger       *slog.Logger
}

// Conn is a UDP socket that forges its source address per packet.
//
// It satisfies quic.OOBCapablePacketConn and the batched-read interface the
// QUIC layer probes for, which is the point of the whole design: the QUIC
// stack keeps using its own optimised path — sendmmsg with UDP segmentation
// offload out, recvmmsg in — and the only thing this type changes is four
// bytes of a control message on the way out.
type Conn struct {
	*net.UDPConn

	pool   *spoof.Pool
	expect map[netip.Addr]struct{}
	batch  *ipv4.PacketConn
	log    *slog.Logger

	buffers BufferReport
	gso     bool

	// Counters. These are read by the health loop, the admin socket and the
	// metrics exporter, and written on the packet path, so they are atomics
	// rather than anything that would need a lock held per packet.
	txPackets, txBytes atomic.Uint64
	rxPackets, rxBytes atomic.Uint64
	rxFiltered         atomic.Uint64
	lastRxNanos        atomic.Int64

	// oobScratch holds control-message buffers for the rare send that has to
	// build a new control message instead of patching one in place.
	oobScratch sync.Pool

	closeOnce sync.Once
}

// Listen opens the socket and applies every option that has to be set before
// the QUIC layer takes it over.
func Listen(opt Options) (*Conn, error) {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	// The QUIC layer probes for GSO support on the socket and honours this
	// environment variable; setting it here keeps one config key in charge.
	if !opt.GSO {
		_ = os.Setenv("QUIC_GO_DISABLE_GSO", "true")
	}
	// We report the buffer sizes ourselves, with the reason they came out the
	// way they did, so the QUIC layer's own advisory warning is noise.
	_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")

	uc, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(opt.Listen))
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", opt.Listen, err)
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		uc.Close()
		return nil, err
	}

	c := &Conn{
		UDPConn: uc,
		pool:    opt.Pool,
		log:     opt.Logger,
	}
	c.oobScratch.New = func() any { b := make([]byte, 0, 128); return &b }

	if opt.Transparent {
		if err := setTransparent(rc); err != nil {
			uc.Close()
			return nil, fmt.Errorf("transport: %w", err)
		}
	}
	rep, err := setBuffers(rc, opt.RcvBuffer, opt.SndBuffer, opt.ForceBuffers)
	if err != nil {
		uc.Close()
		return nil, fmt.Errorf("transport: socket buffers: %w", err)
	}
	c.buffers = rep
	for _, n := range rep.Notes {
		opt.Logger.Warn("socket buffer", "detail", n)
	}
	c.gso = opt.GSO && gsoSupported(rc)

	if len(opt.Expect) > 0 {
		c.expect = make(map[netip.Addr]struct{}, len(opt.Expect))
		for _, a := range opt.Expect {
			c.expect[a.Unmap()] = struct{}{}
		}
	}
	if opt.BatchedReads {
		c.batch = ipv4.NewPacketConn(uc)
	}
	c.lastRxNanos.Store(time.Now().UnixNano())
	return c, nil
}

// Buffers reports how the socket buffers were sized.
func (c *Conn) Buffers() BufferReport { return c.buffers }

// GSOEnabled reports whether segmentation offload is available on this socket.
func (c *Conn) GSOEnabled() bool { return c.gso }

// WriteMsgUDP sends one packet, overriding the source address.
//
// The QUIC layer hands us a control-message buffer that already carries a
// PKTINFO entry (because the socket is bound to a wildcard address) and
// possibly a UDP_SEGMENT entry for offload. The common path here rewrites the
// source inside that existing entry and allocates nothing.
func (c *Conn) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	src, spoofing := c.pool.Current()
	if !spoofing {
		n, on, err := c.UDPConn.WriteMsgUDP(b, oob, addr)
		c.countTx(n, err)
		return n, on, err
	}
	if patchPktinfo(oob, src) {
		n, on, err := c.UDPConn.WriteMsgUDP(b, oob, addr)
		c.countTx(n, err)
		return n, on, err
	}
	sp := c.oobScratch.Get().(*[]byte)
	out := append((*sp)[:0], oob...)
	out = appendPktinfo(out, src)
	n, on, err := c.UDPConn.WriteMsgUDP(b, out, addr)
	*sp = out[:0]
	c.oobScratch.Put(sp)
	c.countTx(n, err)
	return n, on, err
}

// WriteTo is the unbatched fallback the QUIC layer uses when it has no control
// message to send. It has to spoof the source too.
func (c *Conn) WriteTo(b []byte, addr net.Addr) (int, error) {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, fmt.Errorf("transport: unexpected address type %T", addr)
	}
	n, _, err := c.WriteMsgUDP(b, nil, ua)
	return n, err
}

func (c *Conn) countTx(n int, err error) {
	if err != nil {
		return
	}
	c.txPackets.Add(1)
	c.txBytes.Add(uint64(n))
}

// ReadBatch pulls several packets out of the kernel in one recvmmsg. The QUIC
// layer probes for this method and uses it in preference to reading one packet
// at a time, which is where most of the receive-side syscall cost goes.
//
// Note that kernel UDP_GRO is deliberately *not* enabled on this socket: the
// QUIC layer has no way to split a coalesced datagram back into the packets it
// was made from, so turning GRO on would corrupt every read.
func (c *Conn) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	if c.batch == nil {
		return c.readBatchOneByOne(ms, flags)
	}
	n, err := c.batch.ReadBatch(ms, flags)
	if n > 0 {
		c.accept(ms[:n])
	}
	return n, err
}

// readBatchOneByOne serves the same contract without recvmmsg, for the case
// where batched reads are switched off in config.
func (c *Conn) readBatchOneByOne(ms []ipv4.Message, _ int) (int, error) {
	if len(ms) == 0 {
		return 0, nil
	}
	m := &ms[0]
	n, oobn, _, addr, err := c.UDPConn.ReadMsgUDP(m.Buffers[0], m.OOB)
	if err != nil {
		return 0, err
	}
	m.N, m.NN, m.Addr = n, oobn, addr
	c.accept(ms[:1])
	return 1, nil
}

// accept records what arrived and blanks anything from a source we did not
// expect. Blanking rather than compacting is deliberate: the QUIC layer pairs
// each message with a buffer by index, so reordering the batch would
// desynchronise its bookkeeping. A zero-length packet is discarded there.
//
// This filter is an early-out under flood, not a security boundary — a source
// address is forgeable, which is the entire premise of this tunnel. The
// boundary is the AEAD.
func (c *Conn) accept(ms []ipv4.Message) {
	now := time.Now().UnixNano()
	good := 0
	for i := range ms {
		m := &ms[i]
		if m.N <= 0 {
			continue
		}
		if c.expect != nil {
			ua, ok := m.Addr.(*net.UDPAddr)
			if !ok {
				m.N = 0
				c.rxFiltered.Add(1)
				continue
			}
			ip, _ := netip.AddrFromSlice(ua.IP)
			if _, allowed := c.expect[ip.Unmap()]; !allowed {
				m.N = 0
				c.rxFiltered.Add(1)
				continue
			}
		}
		good++
		c.rxPackets.Add(1)
		c.rxBytes.Add(uint64(m.N))
	}
	if good > 0 {
		c.lastRxNanos.Store(now)
		c.pool.MarkGood()
	}
}

// ReadMsgUDP applies the same filter for the unbatched path.
func (c *Conn) ReadMsgUDP(b, oob []byte) (int, int, int, *net.UDPAddr, error) {
	for {
		n, oobn, flags, addr, err := c.UDPConn.ReadMsgUDP(b, oob)
		if err != nil {
			return n, oobn, flags, addr, err
		}
		if c.expect != nil && addr != nil {
			ip, _ := netip.AddrFromSlice(addr.IP)
			if _, ok := c.expect[ip.Unmap()]; !ok {
				c.rxFiltered.Add(1)
				continue
			}
		}
		c.rxPackets.Add(1)
		c.rxBytes.Add(uint64(n))
		c.lastRxNanos.Store(time.Now().UnixNano())
		c.pool.MarkGood()
		return n, oobn, flags, addr, err
	}
}

// SetReadBuffer is a no-op. The buffer was sized at construction from config,
// using the forced socket options where the process has the capability for
// them; letting the QUIC layer resize it afterwards would quietly override
// whatever the tier asked for.
func (c *Conn) SetReadBuffer(int) error { return nil }

// SetWriteBuffer is a no-op for the same reason.
func (c *Conn) SetWriteBuffer(int) error { return nil }

// Counters reports cumulative packets sent and received, for the spoof-source
// health loop.
func (c *Conn) Counters() (tx, rx uint64) {
	return c.txPackets.Load(), c.rxPackets.Load()
}

// Bytes reports cumulative bytes sent and received.
func (c *Conn) Bytes() (tx, rx uint64) {
	return c.txBytes.Load(), c.rxBytes.Load()
}

// Filtered counts packets dropped for arriving from an unexpected source.
func (c *Conn) Filtered() uint64 { return c.rxFiltered.Load() }

// LastRx is when a packet last arrived from an accepted source.
func (c *Conn) LastRx() time.Time { return time.Unix(0, c.lastRxNanos.Load()) }

// Close shuts the socket down once.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.UDPConn.Close() })
	return err
}
