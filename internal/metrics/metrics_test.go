package metrics

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCounterIncrement(t *testing.T) {
	c := &Counter{}
	if c.Load() != 0 {
		t.Errorf("initial value should be 0, got %d", c.Load())
	}

	c.Inc(5)
	if c.Load() != 5 {
		t.Errorf("after Inc(5), expected 5, got %d", c.Load())
	}

	c.Inc(-3)
	if c.Load() != 2 {
		t.Errorf("after Inc(-3), expected 2, got %d", c.Load())
	}
}

func TestGaugeSet(t *testing.T) {
	g := &Gauge{}
	g.Set(42)
	if g.Load() != 42 {
		t.Errorf("expected 42, got %d", g.Load())
	}

	g.Set(100)
	if g.Load() != 100 {
		t.Errorf("expected 100, got %d", g.Load())
	}
}

func TestCollectorRecordMetrics(t *testing.T) {
	collector := NewCollector(nil)

	collector.RecordTx(100, 5000)
	if collector.packetsTx.Load() != 100 {
		t.Errorf("expected 100 packets_tx, got %d", collector.packetsTx.Load())
	}
	if collector.bytesTx.Load() != 5000 {
		t.Errorf("expected 5000 bytes_tx, got %d", collector.bytesTx.Load())
	}

	collector.RecordRx(50, 2500)
	if collector.packetsRx.Load() != 50 {
		t.Errorf("expected 50 packets_rx, got %d", collector.packetsRx.Load())
	}
	if collector.bytesRx.Load() != 2500 {
		t.Errorf("expected 2500 bytes_rx, got %d", collector.bytesRx.Load())
	}
}

func TestCollectorPeerActivity(t *testing.T) {
	collector := NewCollector(nil)

	before := time.Now().Unix()
	collector.RecordPeerActivity(1)
	after := time.Now().Unix()

	collector.mu.Lock()
	lastActivity, exists := collector.peerLastActivity[1]
	collector.mu.Unlock()

	if !exists {
		t.Fatalf("peer activity not recorded")
	}
	if lastActivity < before || lastActivity > after {
		t.Errorf("peer activity timestamp out of range: %d (expected %d-%d)", lastActivity, before, after)
	}
}

func TestExporterMetricsEndpoint(t *testing.T) {
	collector := NewCollector(nil)

	collector.RecordTx(100, 5000)
	collector.RecordRx(50, 2500)
	collector.RecordDatagramSent()
	collector.RecordDatagramReceived()
	collector.UpdateActivePeers(2)

	exporter := NewExporter(collector, "127.0.0.1:0")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := exporter.Listen(ctx); err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer exporter.Close(ctx)

	time.Sleep(100 * time.Millisecond)

	addr := exporter.Addr()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body failed: %v", err)
	}

	output := string(body)

	required := []string{
		"quiccochet_packets_tx",
		"quiccochet_packets_rx",
		"quiccochet_bytes_tx",
		"quiccochet_bytes_rx",
		"quiccochet_datagrams_sent",
		"quiccochet_datagrams_received",
		"quiccochet_peers_active",
	}

	for _, metric := range required {
		if !strings.Contains(output, metric) {
			t.Errorf("metric %s not found in output", metric)
		}
	}

	if !strings.Contains(output, "100") {
		t.Errorf("packets_tx value 100 not found")
	}
	if !strings.Contains(output, "5000") {
		t.Errorf("bytes_tx value 5000 not found")
	}
}

func TestExporterHealthzEndpoint(t *testing.T) {
	collector := NewCollector(nil)

	exporter := NewExporter(collector, "127.0.0.1:0")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := exporter.Listen(ctx); err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer exporter.Close(ctx)

	time.Sleep(100 * time.Millisecond)

	addr := exporter.Addr()
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("get /healthz failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body failed: %v", err)
	}

	if string(body) != "OK" {
		t.Errorf("expected OK, got %s", string(body))
	}
}
