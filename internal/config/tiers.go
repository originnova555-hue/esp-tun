package config

import (
	"fmt"

	"github.com/originnova555-hue/esp-tun/internal/crypto"
)

// Performance tiers. A tier is only a set of defaults over the ordinary config
// surface — there is no tier-specific code path anywhere in the engine.
const (
	TierLight  = "light"  // weak VPS, 1-20 users
	TierMedium = "medium" // 20-100 users
	TierHigh   = "high"   // 100-500 users
	TierUltra  = "ultra"  // thousands of concurrent users
)

// Tiers lists the presets in ascending order of appetite.
var Tiers = []string{TierLight, TierMedium, TierHigh, TierUltra}

// base is the common ground every tier starts from.
func base() *Config {
	return &Config{
		Role: RoleServer,
		TUN: TUN{
			Name:    "qc0",
			MTU:     1360,
			Persist: true,
		},
		Transport: Transport{
			Listen:       "0.0.0.0:6262",
			Transparent:  true,
			ForceBuffers: true,
			GSO:          true,
			GRO:          true,
			Health: Health{
				Enabled:       true,
				IntervalSec:   15,
				TimeoutSec:    5,
				FailThreshold: 3,
				QuarantineSec: 120,
			},
		},
		QUIC: QUIC{
			HandshakeTimeoutSec: 10,
			InitialPacketSize:   1350,
			CongestionControl:   "cubic",
			DatagramQueue:       1024,
			ReconnectMinMs:      500,
			ReconnectMaxMs:      30000,
		},
		Crypto: Crypto{Cipher: "auto", SNI: crypto.DefaultSNI, ALPN: []string{crypto.DefaultALPN}},
		Datapath: Datapath{
			Coalesce:   true,
			CoalesceUS: 100,
			BatchSize:  32,
		},
		Metrics: Metrics{Enabled: true, Listen: "127.0.0.1:9808"},
		Log:     Log{Level: "info"},
	}
}

// Preset returns a fresh Config carrying the named tier's defaults.
func Preset(tier string) (*Config, error) {
	c := base()
	switch tier {
	case TierLight:
		// Weak box: keep the socket cheap and never pad. A single worker keeps
		// the packet path on one core, which is the right call when there is
		// little cache to share in the first place.
		c.QUIC.PoolSize = 2
		c.QUIC.KeepAlivePeriodSec = 15
		c.QUIC.MaxIdleTimeoutSec = 60 // lossy last-mile links need slack
		c.QUIC.CongestionControl = "cubic"
		c.QUIC.ReconnectMinMs = 1000 // back off gently on a flaky link
		c.QUIC.ReconnectMaxMs = 60000
		c.Transport.RcvBufferMB = 4
		c.Transport.SndBufferMB = 4
		c.Datapath.Workers = 1
		c.Datapath.PinCores = false
		c.Datapath.BatchSize = 16
		c.Obfs = Obfs{Mode: "none"}

	case TierMedium:
		c.QUIC.PoolSize = 4
		c.QUIC.KeepAlivePeriodSec = 10
		c.QUIC.MaxIdleTimeoutSec = 45
		c.QUIC.CongestionControl = "auto"
		c.QUIC.ReconnectMinMs = 500
		c.QUIC.ReconnectMaxMs = 30000
		c.Transport.RcvBufferMB = 8
		c.Transport.SndBufferMB = 8
		c.Datapath.Workers = 2
		c.Datapath.PinCores = false
		c.Datapath.BatchSize = 32
		// Size binning only: bucket the frame length so the length histogram
		// stops being a fingerprint, without paying full-MTU padding.
		c.Obfs = Obfs{Mode: "binning", BinSize: 256}

	case TierHigh:
		c.QUIC.PoolSize = 8
		c.QUIC.KeepAlivePeriodSec = 8
		c.QUIC.MaxIdleTimeoutSec = 30
		c.QUIC.CongestionControl = "cubic"
		c.QUIC.ReconnectMinMs = 250
		c.QUIC.ReconnectMaxMs = 15000
		c.Transport.RcvBufferMB = 24
		c.Transport.SndBufferMB = 24
		c.Datapath.Workers = 4
		c.Datapath.PinCores = true
		c.Datapath.BatchSize = 64
		// Left at binning on purpose. Turn this up to "standard" only once
		// active DPI has actually been observed on the path — see docs/TIERS.md.
		c.Obfs = Obfs{Mode: "binning", BinSize: 256}

	case TierUltra:
		c.QUIC.PoolSize = 16
		c.QUIC.KeepAlivePeriodSec = 5
		c.QUIC.MaxIdleTimeoutSec = 30
		c.QUIC.CongestionControl = "cubic"
		c.QUIC.ReconnectMinMs = 200
		c.QUIC.ReconnectMaxMs = 10000
		c.Transport.RcvBufferMB = 32
		c.Transport.SndBufferMB = 32
		c.Transport.ForceBuffers = true
		c.Datapath.Workers = 0 // 0 = one worker per available core
		c.Datapath.PinCores = true
		c.Datapath.BatchSize = 128
		c.Datapath.CoalesceUS = 50
		c.Obfs = Obfs{Mode: "binning", BinSize: 512}

	default:
		return nil, fmt.Errorf("unknown tier %q (want one of %v)", tier, Tiers)
	}
	c.Tier = tier
	return c, nil
}
