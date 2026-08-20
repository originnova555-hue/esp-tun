package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pechenyeru/quiccochet/internal/admin"
)

func TestRenderSpoofIPsEmptyIsNoop(t *testing.T) {
	var buf bytes.Buffer
	renderSpoofIPs(&buf, nil)
	if buf.Len() != 0 {
		t.Fatalf("expected no output for empty spoof IP list, got %q", buf.String())
	}
}

func TestRenderSpoofIPsUntestedNeverLabeledHealthy(t *testing.T) {
	// An entry the pool has never sent through (SentCount == 0) must
	// not be reported as "healthy" — Healthy only means "not currently
	// quarantined", not "confirmed working". Reporting it as healthy
	// would be a false positive for an IP nobody has verified yet.
	var buf bytes.Buffer
	renderSpoofIPs(&buf, []admin.SpoofIPStatus{
		{IP: "10.0.0.9", Healthy: true, SentCount: 0},
	})
	out := buf.String()
	if !strings.Contains(out, "10.0.0.9") {
		t.Fatalf("expected IP in output, got %q", out)
	}
	if !strings.Contains(out, "untested") {
		t.Errorf("expected 'untested' for a never-sent IP, got %q", out)
	}
	if strings.Contains(out, "healthy") {
		t.Errorf("untested IP must not also be labeled healthy, got %q", out)
	}
}

func TestRenderSpoofIPsHealthyWithTraffic(t *testing.T) {
	var buf bytes.Buffer
	renderSpoofIPs(&buf, []admin.SpoofIPStatus{
		{IP: "10.0.0.1", Healthy: true, SentCount: 42, LastSentAgoS: 3},
	})
	out := buf.String()
	if !strings.Contains(out, "10.0.0.1") || !strings.Contains(out, "healthy") {
		t.Errorf("expected healthy IP with traffic, got %q", out)
	}
	if !strings.Contains(out, "sent=42") {
		t.Errorf("expected sent count in output, got %q", out)
	}
}

func TestRenderSpoofIPsQuarantined(t *testing.T) {
	var buf bytes.Buffer
	renderSpoofIPs(&buf, []admin.SpoofIPStatus{
		{IP: "10.0.0.2", Healthy: false, SentCount: 5, CooldownLevel: 2, CooldownLeftS: 45},
	})
	out := buf.String()
	if !strings.Contains(out, "10.0.0.2") || !strings.Contains(out, "quarantined") {
		t.Errorf("expected quarantined IP, got %q", out)
	}
	if strings.Contains(out, "untested") {
		t.Errorf("an IP with real send history must not be labeled untested, got %q", out)
	}
}

func TestRenderSpoofIPsMultipleEntriesAllListed(t *testing.T) {
	var buf bytes.Buffer
	renderSpoofIPs(&buf, []admin.SpoofIPStatus{
		{IP: "10.0.0.1", Healthy: true, SentCount: 10},
		{IP: "10.0.0.2", Healthy: false, SentCount: 3, CooldownLevel: 1, CooldownLeftS: 10},
		{IP: "10.0.0.3", Healthy: true, SentCount: 0},
	})
	out := buf.String()
	for _, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		if !strings.Contains(out, ip) {
			t.Errorf("expected %s in output, got %q", ip, out)
		}
	}
}
