package config

import (
	"strings"
	"testing"
)

const minimal = `
name = "t"
role = "client"
tier = "high"
[tun]
local = "10.20.0.2/30"
peer  = "10.20.0.1"
[transport]
peer = "203.0.113.9:6262"
[crypto]
psk = "0123456789abcdef0123"
`

func TestTierPresetApplies(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.QUIC.PoolSize != 8 {
		t.Errorf("pool_size = %d, want the high-tier default 8", c.QUIC.PoolSize)
	}
	if !c.Datapath.PinCores {
		t.Error("pin_cores should be on at the high tier")
	}
	if c.Transport.RcvBufferMB != 24 {
		t.Errorf("rcv_buffer_mb = %d, want 24", c.Transport.RcvBufferMB)
	}
}

func TestExplicitKeyBeatsPreset(t *testing.T) {
	doc := minimal + "\n[quic]\npool_size = 3\n"
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.QUIC.PoolSize != 3 {
		t.Errorf("pool_size = %d, want the hand-written 3", c.QUIC.PoolSize)
	}
	// A neighbouring key in the same table must keep its preset value.
	if c.QUIC.MaxIdleTimeoutSec != 30 {
		t.Errorf("max_idle_timeout_sec = %d, want the preset 30", c.QUIC.MaxIdleTimeoutSec)
	}
}

func TestExplicitFalseBeatsPresetTrue(t *testing.T) {
	doc := minimal + "\n[datapath]\npin_cores = false\n"
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Datapath.PinCores {
		t.Error("pin_cores = false in the file must override the preset")
	}
}

func TestEveryTierSampleIsValid(t *testing.T) {
	for _, tier := range Tiers {
		for _, role := range []Role{RoleServer, RoleClient} {
			s, err := Sample(SampleOptions{Tier: tier, Role: role})
			if err != nil {
				t.Fatalf("sample %s/%s: %v", tier, role, err)
			}
			// The sample ships a placeholder PSK on purpose; swap in a real
			// one so validation exercises everything else.
			s = strings.Replace(s, "CHANGE-ME-both-sides-must-match", "0123456789abcdef0123", 1)
			c, err := Parse([]byte(s))
			if err != nil {
				t.Fatalf("reparse sample %s/%s: %v", tier, role, err)
			}
			if c.Tier != tier || c.Role != role {
				t.Errorf("sample %s/%s round-tripped as %s/%s", tier, role, c.Tier, c.Role)
			}
		}
	}
}

func TestValidateCatchesMistakes(t *testing.T) {
	cases := map[string]string{
		"missing psk":          "name=\"t\"\nrole=\"server\"\n[tun]\nlocal=\"10.0.0.1/30\"\npeer=\"10.0.0.2\"\n",
		"client without peer":  "name=\"t\"\nrole=\"client\"\n[tun]\nlocal=\"10.0.0.1/30\"\npeer=\"10.0.0.2\"\n[crypto]\npsk=\"0123456789abcdef0123\"\n",
		"bad cipher":           minimal + "\n[crypto]\ncipher=\"rot13\"\n",
		"bad tun cidr":         "name=\"t\"\nrole=\"server\"\n[tun]\nlocal=\"10.0.0.1\"\npeer=\"10.0.0.2\"\n[crypto]\npsk=\"0123456789abcdef0123\"\n",
		"non-power-of-two bin": minimal + "\n[obfuscation]\nmode=\"binning\"\nbin_size=300\n",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestBBRFallsBackWithAWarning(t *testing.T) {
	doc := minimal + "\n[quic]\ncongestion_control = \"bbrv1\"\n"
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.QUIC.CongestionControl != CCCubic {
		t.Errorf("congestion_control = %q, want the cubic fallback", c.QUIC.CongestionControl)
	}
	if len(Warnings()) == 0 {
		t.Error("falling back from bbrv1 should raise a warning")
	}
}

func TestUnknownTierIsRejected(t *testing.T) {
	if _, err := Parse([]byte("tier = \"turbo\"\n")); err == nil {
		t.Error("expected an error for an unknown tier")
	}
}
