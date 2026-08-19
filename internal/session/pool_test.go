package session

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/originnova555-hue/esp-tun/internal/config"
	"github.com/originnova555-hue/esp-tun/internal/crypto"
	"github.com/originnova555-hue/esp-tun/internal/frame"
	"github.com/originnova555-hue/esp-tun/internal/spoof"
	"github.com/originnova555-hue/esp-tun/internal/transport"
)

const testPSK = "test-psk-0123456789abcdef"

func quicCfg(pool int) config.QUIC {
	return config.QUIC{
		PoolSize:            pool,
		KeepAlivePeriodSec:  1,
		MaxIdleTimeoutSec:   10,
		HandshakeTimeoutSec: 5,
		InitialPacketSize:   1350,
		DatagramQueue:       256,
		ReconnectMinMs:      50,
		ReconnectMaxMs:      500,
	}
}

// endpoint is one side of a test tunnel.
type endpoint struct {
	pool *Pool
	conn *transport.Conn
	got  chan []byte
}

// tryEndpoint builds one side of a test tunnel, reporting rather than failing
// so a caller can retry a port that is still being released.
func tryEndpoint(role config.Role, listen, peer netip.AddrPort, poolSize int) (*endpoint, error) {
	sp, err := spoof.NewPool(nil, spoof.Options{})
	if err != nil {
		return nil, err
	}
	tc, err := transport.Listen(transport.Options{
		Listen:       listen,
		Pool:         sp,
		RcvBuffer:    4 << 20,
		SndBuffer:    4 << 20,
		GSO:          true,
		BatchedReads: true,
	})
	if err != nil {
		return nil, err
	}
	id, err := crypto.DeriveIdentity(testPSK, "")
	if err != nil {
		tc.Close()
		return nil, err
	}
	ep := &endpoint{conn: tc, got: make(chan []byte, 8192)}
	p, err := New(Options{
		Role:       role,
		Conn:       tc,
		Peer:       peer,
		ServerTLS:  id.ServerTLS(crypto.TLSOptions{}),
		ClientTLS:  id.ClientTLS(crypto.TLSOptions{}),
		QUIC:       quicCfg(poolSize),
		OnDatagram: func(b []byte) { ep.got <- b },
	})
	if err != nil {
		tc.Close()
		return nil, err
	}
	ep.pool = p
	return ep, nil
}

func newEndpoint(t *testing.T, role config.Role, listen, peer netip.AddrPort, poolSize int) *endpoint {
	t.Helper()
	ep, err := tryEndpoint(role, listen, peer, poolSize)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	t.Cleanup(func() { ep.pool.Close(); ep.conn.Close() })
	return ep
}

func TestDatagramsFlowBothWays(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	server := newEndpoint(t, config.RoleServer, netip.AddrPortFrom(lo, 0), netip.AddrPort{}, 2)
	srvAddr := server.conn.LocalAddr().(*net.UDPAddr)
	client := newEndpoint(t, config.RoleClient, netip.AddrPortFrom(lo, 0),
		netip.AddrPortFrom(lo, uint16(srvAddr.Port)), 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.pool.Run(ctx)
	go client.pool.Run(ctx)

	waitReady(t, client.pool, 5*time.Second)
	waitReady(t, server.pool, 5*time.Second)

	// client -> server
	payload, _ := frame.Append(nil, frame.TypeIP, []byte("up the tunnel"))
	if err := client.pool.Send(payload, 0); err != nil {
		t.Fatalf("client send: %v", err)
	}
	assertFrame(t, server.got, "up the tunnel")

	// server -> client
	payload, _ = frame.Append(nil, frame.TypeIP, []byte("and back down"))
	if err := server.pool.Send(payload, 0); err != nil {
		t.Fatalf("server send: %v", err)
	}
	assertFrame(t, client.got, "and back down")
}

func TestEveryPoolSlotConnects(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	const n = 4
	server := newEndpoint(t, config.RoleServer, netip.AddrPortFrom(lo, 0), netip.AddrPort{}, n)
	srvAddr := server.conn.LocalAddr().(*net.UDPAddr)
	client := newEndpoint(t, config.RoleClient, netip.AddrPortFrom(lo, 0),
		netip.AddrPortFrom(lo, uint16(srvAddr.Port)), n)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.pool.Run(ctx)
	go client.pool.Run(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if client.pool.Live() == n && server.pool.Live() == n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("only %d/%d client and %d/%d server slots came up",
		client.pool.Live(), n, server.pool.Live(), n)
}

func TestFlowHashPinsAFlowToOneSlot(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	const n = 4
	server := newEndpoint(t, config.RoleServer, netip.AddrPortFrom(lo, 0), netip.AddrPort{}, n)
	srvAddr := server.conn.LocalAddr().(*net.UDPAddr)
	client := newEndpoint(t, config.RoleClient, netip.AddrPortFrom(lo, 0),
		netip.AddrPortFrom(lo, uint16(srvAddr.Port)), n)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.pool.Run(ctx)
	go client.pool.Run(ctx)
	waitReady(t, client.pool, 5*time.Second)
	waitAllLive(t, client.pool, n, 10*time.Second)

	// Everything on one flow hash must arrive in the order it was sent: that
	// is the whole reason the pool hashes instead of round-robins.
	const count = 200
	for i := range count {
		b, _ := frame.Append(nil, frame.TypeIP, []byte{byte(i)})
		if err := client.pool.Send(b, 7); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	got := make([]byte, 0, count)
	deadline := time.After(10 * time.Second)
	for len(got) < count {
		select {
		case d := <-server.got:
			if err := frame.Walk(d, func(_ byte, payload []byte) error {
				got = append(got, payload[0])
				return nil
			}); err != nil {
				t.Fatalf("walk: %v", err)
			}
		case <-deadline:
			t.Fatalf("received %d of %d datagrams", len(got), count)
		}
	}
	for i := range got {
		if got[i] != byte(i) {
			t.Fatalf("datagram %d out of order: got %d — a single flow must stay ordered",
				i, got[i])
		}
	}
}

func TestSendWithoutASessionReportsIt(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	client := newEndpoint(t, config.RoleClient, netip.AddrPortFrom(lo, 0),
		netip.AddrPortFrom(lo, 1), 2) // nothing is listening on port 1
	if err := client.pool.Send([]byte("x"), 0); !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

func TestWrongPSKIsRefused(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	server := newEndpoint(t, config.RoleServer, netip.AddrPortFrom(lo, 0), netip.AddrPort{}, 1)
	srvAddr := server.conn.LocalAddr().(*net.UDPAddr)

	sp, _ := spoof.NewPool(nil, spoof.Options{})
	tc, err := transport.Listen(transport.Options{
		Listen: netip.AddrPortFrom(lo, 0), Pool: sp,
		RcvBuffer: 1 << 20, SndBuffer: 1 << 20, BatchedReads: true,
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	defer tc.Close()
	other, err := crypto.DeriveIdentity("a-completely-different-secret", "")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	client, err := New(Options{
		Role: config.RoleClient, Conn: tc,
		Peer:       netip.AddrPortFrom(lo, uint16(srvAddr.Port)),
		ClientTLS:  other.ClientTLS(crypto.TLSOptions{}),
		ServerTLS:  other.ServerTLS(crypto.TLSOptions{}),
		QUIC:       quicCfg(1),
		OnDatagram: func([]byte) {},
	})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.pool.Run(ctx)
	go client.Run(ctx)

	time.Sleep(2 * time.Second)
	if client.Ready() {
		t.Error("a peer holding a different pre-shared key must not get a session")
	}
	if client.Stats().HandshakeFailed == 0 {
		t.Error("the refused handshake should have been counted")
	}
}

func TestClientReconnectsAfterTheServerRestarts(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	server := newEndpoint(t, config.RoleServer, netip.AddrPortFrom(lo, 0), netip.AddrPort{}, 1)
	srvAddr := server.conn.LocalAddr().(*net.UDPAddr)
	client := newEndpoint(t, config.RoleClient, netip.AddrPortFrom(lo, 0),
		netip.AddrPortFrom(lo, uint16(srvAddr.Port)), 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx, scancel := context.WithCancel(ctx)
	go server.pool.Run(sctx)
	go client.pool.Run(ctx)
	waitReady(t, client.pool, 5*time.Second)

	// Take the server down the way a restart would, releasing its socket.
	scancel()
	server.pool.Close()
	server.conn.Close()

	// Bring a fresh server up on the same port.
	server2 := newEndpointRetry(t, config.RoleServer,
		netip.AddrPortFrom(lo, uint16(srvAddr.Port)), netip.AddrPort{}, 1)
	go server2.pool.Run(ctx)

	waitReady(t, server2.pool, 15*time.Second)

	b, _ := frame.Append(nil, frame.TypeIP, []byte("after the restart"))
	deadline := time.After(10 * time.Second)
	for {
		if err := client.pool.Send(b, 0); err == nil {
			select {
			case d := <-server2.got:
				_ = d
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		select {
		case <-deadline:
			t.Fatal("client never re-established a session with the restarted server")
		default:
		}
	}
}

// TestBurstIsAccountedForExactly checks that a burst larger than the queues
// drops rather than blocks, and that every dropped datagram is counted. The
// drop itself is the design: blocking here would stall the reader pulling
// packets off the TUN interface, turning one congested connection into a
// stall for every flow on the tunnel.
func TestBurstIsAccountedForExactly(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	const n = 4
	server := newEndpoint(t, config.RoleServer, netip.AddrPortFrom(lo, 0), netip.AddrPort{}, n)
	srvAddr := server.conn.LocalAddr().(*net.UDPAddr)
	client := newEndpoint(t, config.RoleClient, netip.AddrPortFrom(lo, 0),
		netip.AddrPortFrom(lo, uint16(srvAddr.Port)), n)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.pool.Run(ctx)
	go client.pool.Run(ctx)
	waitAllLive(t, client.pool, n, 10*time.Second)

	var received atomic.Int64
	go func() {
		for {
			select {
			case <-server.got:
				received.Add(1)
			case <-ctx.Done():
				return
			}
		}
	}()

	base := client.pool.Stats()
	const total = 1600
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range total / 8 {
				b, _ := frame.Append(nil, frame.TypeIP, []byte{byte(w), byte(i)})
				_ = client.pool.Send(b, uint32(w))
			}
		}(w)
	}
	wg.Wait()

	// Wait for the queues to drain so the counters settle.
	var st Stats
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st = client.pool.Stats()
		if (st.Sent-base.Sent)+(st.DroppedQueueFull-base.DroppedQueueFull) >= total {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sent := st.Sent - base.Sent
	dropped := st.DroppedQueueFull - base.DroppedQueueFull
	if sent+dropped != total {
		t.Errorf("sent %d + dropped %d = %d, want exactly %d — datagrams must not vanish unaccounted",
			sent, dropped, sent+dropped, total)
	}
	if sent == 0 {
		t.Error("nothing was sent at all")
	}

	for time.Now().Before(deadline) && uint64(received.Load()) < sent {
		time.Sleep(20 * time.Millisecond)
	}
	if got := uint64(received.Load()); got < sent {
		t.Errorf("server received %d of the %d datagrams the client says it sent", got, sent)
	}
}

// newEndpointRetry is newEndpoint with patience for a port the kernel has not
// finished releasing yet.
func newEndpointRetry(t *testing.T, role config.Role, listen, peer netip.AddrPort, poolSize int) *endpoint {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ep, err := tryEndpoint(role, listen, peer, poolSize)
		if err == nil {
			t.Cleanup(func() { ep.pool.Close(); ep.conn.Close() })
			return ep
		}
		if time.Now().After(deadline) {
			t.Fatalf("endpoint: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitReady(t *testing.T, p *Pool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if p.Ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no session came up within %s (stats: %+v)", d, p.Stats())
}

func waitAllLive(t *testing.T, p *Pool, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if p.Live() >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("only %d/%d slots came up within %s", p.Live(), n, d)
}

// assertFrame waits for a datagram carrying a real frame. Every new link
// sends a padding-only datagram first to learn the peer's datagram ceiling,
// and the datapath skips those, so the test has to as well.
func assertFrame(t *testing.T, ch chan []byte, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case d := <-ch:
			var got string
			var seen bool
			if err := frame.Walk(d, func(_ byte, payload []byte) error {
				got, seen = string(payload), true
				return nil
			}); err != nil {
				t.Fatalf("walk: %v", err)
			}
			if !seen {
				continue // a size probe; keep waiting for real traffic
			}
			if got != want {
				t.Errorf("payload = %q, want %q", got, want)
			}
			return
		case <-deadline:
			t.Fatalf("no datagram arrived carrying %q", want)
		}
	}
}
