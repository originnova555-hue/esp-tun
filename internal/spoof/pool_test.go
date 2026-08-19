package spoof

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// fakeActivity lets a test drive the two signals the health loop watches.
type fakeActivity struct {
	tx, rx atomic.Uint64
	lastRx atomic.Int64
}

func (f *fakeActivity) Counters() (uint64, uint64) { return f.tx.Load(), f.rx.Load() }
func (f *fakeActivity) LastRx() time.Time          { return time.Unix(0, f.lastRx.Load()) }
func (f *fakeActivity) send(n uint64)              { f.tx.Add(n) }
func (f *fakeActivity) hear()                      { f.lastRx.Store(time.Now().UnixNano()) }

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestEmptyPoolMeansNoSpoofing(t *testing.T) {
	p, err := NewPool(nil, Options{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, ok := p.Current(); ok {
		t.Error("an empty pool must report that there is nothing to spoof with")
	}
}

func TestDuplicateSourcesCollapse(t *testing.T) {
	p, err := NewPool(addrs("192.0.2.1", "192.0.2.1", "192.0.2.2"), Options{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if p.Len() != 2 {
		t.Errorf("pool holds %d sources, want 2 after collapsing the duplicate", p.Len())
	}
}

func TestRotateSkipsQuarantinedSource(t *testing.T) {
	p, _ := NewPool(addrs("192.0.2.1", "192.0.2.2", "192.0.2.3"), Options{})
	// Quarantine the one we would rotate to next.
	p.sources[1].quarantinedUntil = time.Now().Add(time.Hour)

	if !p.ForceRotate("test") {
		t.Fatal("rotate returned false with two healthy sources available")
	}
	got, _ := p.Current()
	if got != netip.MustParseAddr("192.0.2.3") {
		t.Errorf("rotated to %v, want 192.0.2.3 — 192.0.2.2 is quarantined", got)
	}
}

func TestRotateFallsBackWhenEverythingIsQuarantined(t *testing.T) {
	p, _ := NewPool(addrs("192.0.2.1", "192.0.2.2", "192.0.2.3"), Options{})
	now := time.Now()
	p.sources[1].quarantinedUntil = now.Add(time.Hour)
	p.sources[2].quarantinedUntil = now.Add(time.Minute) // recovers soonest

	if !p.ForceRotate("test") {
		t.Fatal("rotate should still move when every candidate is quarantined")
	}
	got, _ := p.Current()
	if got != netip.MustParseAddr("192.0.2.3") {
		t.Errorf("rotated to %v, want the source that recovers soonest (192.0.2.3)", got)
	}
}

func TestSingleSourceNeverRotates(t *testing.T) {
	p, _ := NewPool(addrs("192.0.2.1"), Options{})
	if p.ForceRotate("test") {
		t.Error("a one-address pool has nowhere to rotate to")
	}
}

func TestHealthLoopQuarantinesAfterThreshold(t *testing.T) {
	rotated := make(chan [2]netip.Addr, 4)
	p, _ := NewPool(addrs("192.0.2.1", "192.0.2.2"), Options{
		Enabled:       true,
		Interval:      20 * time.Millisecond,
		Timeout:       10 * time.Millisecond,
		FailThreshold: 2,
		Quarantine:    time.Hour,
		OnRotate:      func(from, to netip.Addr) { rotated <- [2]netip.Addr{from, to} },
	})

	act := &fakeActivity{}
	act.lastRx.Store(time.Now().Add(-time.Hour).UnixNano()) // silent for ages

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx, act)

	// Keep sending so the loop sees traffic going out with nothing coming back.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				act.send(10)
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	select {
	case ev := <-rotated:
		if ev[0] != netip.MustParseAddr("192.0.2.1") || ev[1] != netip.MustParseAddr("192.0.2.2") {
			t.Errorf("rotated %v -> %v, want 192.0.2.1 -> 192.0.2.2", ev[0], ev[1])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("health loop did not rotate away from the silent source")
	}

	var quarantined bool
	for _, s := range p.Snapshot() {
		if s.Addr == netip.MustParseAddr("192.0.2.1") && s.Quarantined {
			quarantined = true
		}
	}
	if !quarantined {
		t.Error("the failing source should have been quarantined")
	}
}

func TestHealthLoopIgnoresAnIdleTunnel(t *testing.T) {
	rotated := make(chan struct{}, 1)
	p, _ := NewPool(addrs("192.0.2.1", "192.0.2.2"), Options{
		Enabled:       true,
		Interval:      10 * time.Millisecond,
		Timeout:       5 * time.Millisecond,
		FailThreshold: 1,
		Quarantine:    time.Hour,
		OnRotate:      func(netip.Addr, netip.Addr) { rotated <- struct{}{} },
	})
	act := &fakeActivity{}
	act.lastRx.Store(time.Now().Add(-time.Hour).UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx, act)

	// No sends at all: silence from the peer means nothing here.
	select {
	case <-rotated:
		t.Fatal("an idle tunnel must not be read as a dead spoof source")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestHealthLoopClearsStreakWhenThePeerAnswers(t *testing.T) {
	p, _ := NewPool(addrs("192.0.2.1", "192.0.2.2"), Options{
		Enabled:       true,
		Interval:      10 * time.Millisecond,
		Timeout:       time.Second,
		FailThreshold: 2,
		Quarantine:    time.Hour,
	})
	act := &fakeActivity{}
	act.hear()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx, act)

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		act.send(5)
		act.hear()
		time.Sleep(5 * time.Millisecond)
	}
	if p.Rotations() != 0 {
		t.Errorf("rotations = %d, want 0 while the peer keeps answering", p.Rotations())
	}
}
