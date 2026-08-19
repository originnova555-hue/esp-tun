package metrics

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/originnova555-hue/esp-tun/internal/session"
)

type Counter struct {
	value atomic.Int64
}

func (c *Counter) Inc(delta int64) {
	c.value.Add(delta)
}

func (c *Counter) Load() int64 {
	return c.value.Load()
}

type Gauge struct {
	value atomic.Int64
}

func (g *Gauge) Set(v int64) {
	g.value.Store(v)
}

func (g *Gauge) Load() int64 {
	return g.value.Load()
}

type Collector struct {
	pool *session.Pool

	packetsTx         *Counter
	packetsRx         *Counter
	bytesTx           *Counter
	bytesRx           *Counter
	datagramsSent     *Counter
	datagramsReceived *Counter
	peersActive       *Gauge
	peerLastActivity  map[int]int64
	mu                sync.Mutex
}

func NewCollector(pool *session.Pool) *Collector {
	return &Collector{
		pool:             pool,
		packetsTx:        &Counter{},
		packetsRx:        &Counter{},
		bytesTx:          &Counter{},
		bytesRx:          &Counter{},
		datagramsSent:    &Counter{},
		datagramsReceived: &Counter{},
		peersActive:      &Gauge{},
		peerLastActivity: make(map[int]int64),
	}
}

func (c *Collector) RecordTx(packets, bytes int64) {
	c.packetsTx.Inc(packets)
	c.bytesTx.Inc(bytes)
}

func (c *Collector) RecordRx(packets, bytes int64) {
	c.packetsRx.Inc(packets)
	c.bytesRx.Inc(bytes)
}

func (c *Collector) RecordDatagramSent() {
	c.datagramsSent.Inc(1)
}

func (c *Collector) RecordDatagramReceived() {
	c.datagramsReceived.Inc(1)
}

func (c *Collector) RecordPeerActivity(peerID int) {
	c.mu.Lock()
	c.peerLastActivity[peerID] = time.Now().Unix()
	c.mu.Unlock()
}

func (c *Collector) UpdateActivePeers(count int) {
	c.peersActive.Set(int64(count))
}

type Exporter struct {
	collector  *Collector
	listenAddr string
	server     *http.Server
	ln         net.Listener
	shutdown   chan struct{}
}

func NewExporter(collector *Collector, listenAddr string) *Exporter {
	return &Exporter{
		collector:  collector,
		listenAddr: listenAddr,
		shutdown:   make(chan struct{}),
	}
}

func (e *Exporter) Addr() string {
	if e.ln != nil {
		return e.ln.Addr().String()
	}
	return e.listenAddr
}

func (e *Exporter) Listen(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", e.handleMetrics)
	mux.HandleFunc("/healthz", e.handleHealthz)

	e.server = &http.Server{
		Addr:    e.listenAddr,
		Handler: mux,
	}

	ln, err := net.Listen("tcp", e.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", e.listenAddr, err)
	}
	e.ln = ln

	go func() {
		if err := e.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[metrics] serve error: %v\n", err)
		}
	}()

	return nil
}

func (e *Exporter) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	fmt.Fprintf(w, "# HELP quiccochet_packets_tx Total packets transmitted\n")
	fmt.Fprintf(w, "# TYPE quiccochet_packets_tx counter\n")
	fmt.Fprintf(w, "quiccochet_packets_tx %d\n\n", e.collector.packetsTx.Load())

	fmt.Fprintf(w, "# HELP quiccochet_packets_rx Total packets received\n")
	fmt.Fprintf(w, "# TYPE quiccochet_packets_rx counter\n")
	fmt.Fprintf(w, "quiccochet_packets_rx %d\n\n", e.collector.packetsRx.Load())

	fmt.Fprintf(w, "# HELP quiccochet_bytes_tx Total bytes transmitted\n")
	fmt.Fprintf(w, "# TYPE quiccochet_bytes_tx counter\n")
	fmt.Fprintf(w, "quiccochet_bytes_tx %d\n\n", e.collector.bytesTx.Load())

	fmt.Fprintf(w, "# HELP quiccochet_bytes_rx Total bytes received\n")
	fmt.Fprintf(w, "# TYPE quiccochet_bytes_rx counter\n")
	fmt.Fprintf(w, "quiccochet_bytes_rx %d\n\n", e.collector.bytesRx.Load())

	fmt.Fprintf(w, "# HELP quiccochet_datagrams_sent Total QUIC datagrams sent\n")
	fmt.Fprintf(w, "# TYPE quiccochet_datagrams_sent counter\n")
	fmt.Fprintf(w, "quiccochet_datagrams_sent %d\n\n", e.collector.datagramsSent.Load())

	fmt.Fprintf(w, "# HELP quiccochet_datagrams_received Total QUIC datagrams received\n")
	fmt.Fprintf(w, "# TYPE quiccochet_datagrams_received counter\n")
	fmt.Fprintf(w, "quiccochet_datagrams_received %d\n\n", e.collector.datagramsReceived.Load())

	fmt.Fprintf(w, "# HELP quiccochet_peers_active Number of active peers\n")
	fmt.Fprintf(w, "# TYPE quiccochet_peers_active gauge\n")
	fmt.Fprintf(w, "quiccochet_peers_active %d\n\n", e.collector.peersActive.Load())

	e.collector.mu.Lock()
	for peerID, timestamp := range e.collector.peerLastActivity {
		age := time.Now().Unix() - timestamp
		fmt.Fprintf(w, "quiccochet_peer_last_activity_seconds{peer=\"%d\"} %d\n", peerID, age)
	}
	e.collector.mu.Unlock()
}

func (e *Exporter) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

func (e *Exporter) Close(ctx context.Context) error {
	close(e.shutdown)
	if e.server != nil {
		return e.server.Shutdown(ctx)
	}
	return nil
}
