package config

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Cipher choices.
const (
	CipherAuto     = "auto"
	CipherAESGCM   = "aes-256-gcm"
	CipherChaCha20 = "chacha20-poly1305"
)

// Obfuscation modes.
const (
	ObfsNone     = "none"
	ObfsBinning  = "binning"
	ObfsStandard = "standard"
	ObfsParanoid = "paranoid"
)

// Congestion-control selections accepted in config.
const (
	CCCubic = "cubic"
	CCAuto  = "auto"
	CCBBR   = "bbrv1"
)

// warnings accumulated during validation, surfaced by the caller at startup.
type warnList []string

var configWarnings warnList

// Warnings returns notes raised by the last Validate call. These are not
// errors: the config loaded, but something in it did not mean what it says.
func Warnings() []string { return configWarnings }

// Validate checks the config for internal consistency and fills in anything
// that can only be derived once every key is known.
func (c *Config) Validate() error {
	configWarnings = nil
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Name == "" {
		bad("name is required")
	}
	switch c.Role {
	case RoleServer, RoleClient:
	default:
		bad("role must be %q or %q, got %q", RoleServer, RoleClient, c.Role)
	}

	// TUN ---------------------------------------------------------------
	if c.TUN.Name == "" {
		bad("tun.name is required")
	}
	if len(c.TUN.Name) > 15 {
		bad("tun.name %q is longer than the kernel's 15-character limit", c.TUN.Name)
	}
	if c.TUN.Local == "" {
		bad("tun.local is required (CIDR, e.g. \"10.20.0.1/30\")")
	} else if _, err := netip.ParsePrefix(c.TUN.Local); err != nil {
		bad("tun.local: %v", err)
	}
	if c.TUN.Peer == "" {
		bad("tun.peer is required (e.g. \"10.20.0.2\")")
	} else if _, err := netip.ParseAddr(c.TUN.Peer); err != nil {
		bad("tun.peer: %v", err)
	}
	if c.TUN.MTU < 576 || c.TUN.MTU > 9000 {
		bad("tun.mtu %d is outside the usable range 576-9000", c.TUN.MTU)
	}

	// Transport ---------------------------------------------------------
	if c.Transport.Listen == "" {
		bad("transport.listen is required")
	} else if _, err := netip.ParseAddrPort(c.Transport.Listen); err != nil {
		bad("transport.listen: %v", err)
	}
	if c.Role == RoleClient && c.Transport.Peer == "" {
		bad("transport.peer is required for role = \"client\" (the address this side dials)")
	}
	if c.Transport.Peer != "" {
		if _, err := netip.ParseAddrPort(c.Transport.Peer); err != nil {
			bad("transport.peer: %v", err)
		}
	}
	for i, s := range c.Transport.SpoofSrc {
		if _, err := netip.ParseAddr(s); err != nil {
			bad("transport.spoof_src[%d] %q: %v", i, s, err)
		}
	}
	for i, s := range c.Transport.SpoofExpect {
		if _, err := netip.ParseAddr(s); err != nil {
			bad("transport.spoof_expect[%d] %q: %v", i, s, err)
		}
	}
	if len(c.Transport.SpoofSrc) > 0 && !c.Transport.Transparent {
		configWarnings = append(configWarnings,
			"transport.spoof_src is set but transport.transparent = false; "+
				"the kernel will reject a non-local source address without IP_TRANSPARENT")
	}
	if c.Transport.RcvBufferMB <= 0 {
		bad("transport.rcv_buffer_mb must be positive")
	}
	if c.Transport.SndBufferMB <= 0 {
		bad("transport.snd_buffer_mb must be positive")
	}

	// Health ------------------------------------------------------------
	if c.Transport.Health.Enabled {
		if c.Transport.Health.IntervalSec <= 0 {
			bad("transport.health.interval_sec must be positive when health checking is enabled")
		}
		if c.Transport.Health.TimeoutSec <= 0 {
			bad("transport.health.timeout_sec must be positive when health checking is enabled")
		}
		if c.Transport.Health.TimeoutSec >= c.Transport.Health.IntervalSec {
			configWarnings = append(configWarnings,
				"transport.health.timeout_sec >= interval_sec; probes will overlap")
		}
		if c.Transport.Health.FailThreshold <= 0 {
			bad("transport.health.fail_threshold must be positive")
		}
	}

	// QUIC --------------------------------------------------------------
	if c.QUIC.PoolSize < 1 {
		bad("quic.pool_size must be at least 1")
	}
	if c.QUIC.MaxIdleTimeoutSec <= 0 {
		bad("quic.max_idle_timeout_sec must be positive")
	}
	if c.QUIC.KeepAlivePeriodSec > 0 && c.QUIC.KeepAlivePeriodSec*2 >= c.QUIC.MaxIdleTimeoutSec {
		configWarnings = append(configWarnings,
			"quic.keep_alive_period_sec is more than half of max_idle_timeout_sec; "+
				"a single lost keep-alive can then time the session out")
	}
	if c.QUIC.InitialPacketSize < 1200 {
		bad("quic.initial_packet_size must be at least 1200 (QUIC's floor)")
	}
	if c.QUIC.DatagramQueue < 1 {
		bad("quic.datagram_queue must be positive")
	}
	if c.QUIC.ReconnectMinMs <= 0 || c.QUIC.ReconnectMaxMs < c.QUIC.ReconnectMinMs {
		bad("quic.reconnect_min_ms/reconnect_max_ms must be positive with max >= min")
	}
	switch c.QUIC.CongestionControl {
	case CCCubic, CCAuto, "":
	case CCBBR, "bbr":
		// The QUIC stack in use ships Cubic only; there is no hook to swap the
		// sender out. Say so plainly rather than pretend the knob took effect.
		configWarnings = append(configWarnings,
			"quic.congestion_control = \"bbrv1\" is not available in this build; "+
				"falling back to cubic (see docs/CONGESTION.md)")
		c.QUIC.CongestionControl = CCCubic
	default:
		bad("quic.congestion_control %q is not one of cubic, auto, bbrv1", c.QUIC.CongestionControl)
	}

	// Crypto ------------------------------------------------------------
	switch c.Crypto.Cipher {
	case CipherAuto, CipherAESGCM, CipherChaCha20:
	default:
		bad("crypto.cipher %q is not one of %s, %s, %s",
			c.Crypto.Cipher, CipherAuto, CipherAESGCM, CipherChaCha20)
	}
	if strings.TrimSpace(c.Crypto.PSK) == "" {
		bad("crypto.psk is required; both sides must carry the same value")
	} else if len(c.Crypto.PSK) < 16 {
		bad("crypto.psk is shorter than 16 characters")
	}

	// Obfuscation -------------------------------------------------------
	switch c.Obfs.Mode {
	case ObfsNone:
	case ObfsBinning, ObfsStandard, ObfsParanoid:
		if c.Obfs.BinSize <= 0 {
			c.Obfs.BinSize = 256
		}
		if c.Obfs.BinSize&(c.Obfs.BinSize-1) != 0 {
			bad("obfuscation.bin_size %d must be a power of two", c.Obfs.BinSize)
		}
	default:
		bad("obfuscation.mode %q is not one of %s, %s, %s, %s",
			c.Obfs.Mode, ObfsNone, ObfsBinning, ObfsStandard, ObfsParanoid)
	}
	if c.Obfs.ChaffPPS > 0 && c.Obfs.Mode == ObfsNone {
		configWarnings = append(configWarnings,
			"obfuscation.chaff_pps is set but mode = \"none\"; no chaff will be sent")
	}

	// Datapath ----------------------------------------------------------
	if c.Datapath.Workers < 0 {
		bad("datapath.workers cannot be negative (use 0 for one per core)")
	}
	if c.Datapath.BatchSize < 1 {
		bad("datapath.batch_size must be positive")
	}
	if c.Datapath.PinCores && len(c.Datapath.Cores) > 0 && c.Datapath.Workers > 0 &&
		len(c.Datapath.Cores) < c.Datapath.Workers {
		configWarnings = append(configWarnings,
			"datapath.cores lists fewer cores than datapath.workers; workers will share cores")
	}

	// Forwards ----------------------------------------------------------
	for i, f := range c.Forwards {
		switch strings.ToLower(f.Proto) {
		case "tcp", "udp", "both", "":
		default:
			bad("forward[%d].proto %q is not one of tcp, udp, both", i, f.Proto)
		}
		if len(f.Ports) == 0 {
			bad("forward[%d] lists no ports", i)
		}
	}

	return errors.Join(errs...)
}
