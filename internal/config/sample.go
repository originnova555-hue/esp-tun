package config

import (
	"fmt"
	"strings"
)

// SampleOptions steers the generated starter config.
type SampleOptions struct {
	Tier string
	Role Role
	Name string
}

// Sample renders a fully commented starter config for a tier. It is generated
// from the preset itself, so a sample can never drift away from the defaults
// the engine actually applies.
func Sample(opt SampleOptions) (string, error) {
	if opt.Tier == "" {
		opt.Tier = TierMedium
	}
	if opt.Role == "" {
		opt.Role = RoleServer
	}
	c, err := Preset(opt.Tier)
	if err != nil {
		return "", err
	}

	name := opt.Name
	tunLocal, tunPeer := "10.20.0.1/30", "10.20.0.2"
	spoofSrc, spoofExpect := "62.60.212.216", "5.34.222.2"
	peerLine := `# peer         = "203.0.113.9:6262"   # only the client dials`
	if opt.Role == RoleClient {
		if name == "" {
			name = "foreign-main"
		}
		tunLocal, tunPeer = "10.20.0.2/30", "10.20.0.1"
		spoofSrc, spoofExpect = "5.34.222.2", "62.60.212.216"
		peerLine = `peer           = "203.0.113.9:6262"   # real address of the other side`
	} else if name == "" {
		name = "iran-main"
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w(`# quiccochet tunnel — %s (%s), tier "%s"`, name, opt.Role, opt.Tier)
	w(`#`)
	w(`# Every key below is optional: leaving it out gives you the tier default`)
	w(`# shown as the value. Anything you write here wins over the preset.`)
	w(``)
	w(`name = "%s"`, name)
	w(`role = "%s"        # server listens, client dials`, opt.Role)
	w(`tier = "%s"        # light | medium | high | ultra`, opt.Tier)
	w(``)
	w(`# ── Layer-3 interface ────────────────────────────────────────────────`)
	w(`# Created once at startup and kept up for the process lifetime. A session`)
	w(`# drop never tears it down, so routes and iptables rules stay valid.`)
	w(`[tun]`)
	w(`name    = "%s"`, c.TUN.Name)
	w(`local   = "%s"`, tunLocal)
	w(`peer    = "%s"`, tunPeer)
	w(`mtu     = %d`, c.TUN.MTU)
	w(`queues  = %d               # multiqueue fds; 0 = one per worker`, c.TUN.Queues)
	w(`persist = %t`, c.TUN.Persist)
	w(``)
	w(`# ── Spoofed UDP transport ────────────────────────────────────────────`)
	w(`[transport]`)
	w(`listen         = "%s"`, c.Transport.Listen)
	w(`%s`, peerLine)
	w(`# Addresses we send FROM. The kernel builds the IP header; the source is`)
	w(`# overridden per packet with an IP_PKTINFO cmsg, which keeps GSO and`)
	w(`# checksum offload intact (unlike a raw IP_HDRINCL socket).`)
	w(`spoof_src      = ["%s"]`, spoofSrc)
	w(`# Addresses we accept traffic FROM. Empty means accept any source.`)
	w(`spoof_expect   = ["%s"]`, spoofExpect)
	w(`transparent    = %t             # IP_TRANSPARENT; required for a non-local source`, c.Transport.Transparent)
	w(`rcv_buffer_mb  = %d`, c.Transport.RcvBufferMB)
	w(`snd_buffer_mb  = %d`, c.Transport.SndBufferMB)
	w(`force_buffers  = %t             # try SO_RCVBUFFORCE before the rmem_max cap`, c.Transport.ForceBuffers)
	w(`gso            = %t`, c.Transport.GSO)
	w(`gro            = %t`, c.Transport.GRO)
	w(``)
	w(`# Probe each spoof source in turn; quarantine one that stops getting`)
	w(`# replies so a single blackholed address cannot take the tunnel down.`)
	w(`[transport.health]`)
	w(`enabled        = %t`, c.Transport.Health.Enabled)
	w(`interval_sec   = %d`, c.Transport.Health.IntervalSec)
	w(`timeout_sec    = %d`, c.Transport.Health.TimeoutSec)
	w(`fail_threshold = %d`, c.Transport.Health.FailThreshold)
	w(`quarantine_sec = %d`, c.Transport.Health.QuarantineSec)
	w(``)
	w(`# ── QUIC session pool ────────────────────────────────────────────────`)
	w(`# Payload rides in DATAGRAM frames (RFC 9221), never in streams: a lost`)
	w(`# packet is not retransmitted underneath the inner flow's own recovery.`)
	w(`[quic]`)
	w(`pool_size             = %d`, c.QUIC.PoolSize)
	w(`keep_alive_period_sec = %d`, c.QUIC.KeepAlivePeriodSec)
	w(`max_idle_timeout_sec  = %d`, c.QUIC.MaxIdleTimeoutSec)
	w(`handshake_timeout_sec = %d`, c.QUIC.HandshakeTimeoutSec)
	w(`initial_packet_size   = %d`, c.QUIC.InitialPacketSize)
	w(`disable_pmtud         = %t`, c.QUIC.DisablePMTUD)
	w(`congestion_control    = "%s"      # cubic | auto  (see docs/CONGESTION.md)`, c.QUIC.CongestionControl)
	w(`datagram_queue        = %d`, c.QUIC.DatagramQueue)
	w(`reconnect_min_ms      = %d`, c.QUIC.ReconnectMinMs)
	w(`reconnect_max_ms      = %d`, c.QUIC.ReconnectMaxMs)
	w(``)
	w(`# ── Crypto ───────────────────────────────────────────────────────────`)
	w(`[crypto]`)
	w(`# "auto" picks AES-256-GCM when the CPU has AES-NI, ChaCha20-Poly1305`)
	w(`# otherwise. Run "quiccochet cpuinfo" to see what this box will choose.`)
	w(`cipher = "%s"`, c.Crypto.Cipher)
	w(`# Same value on both sides. Generate one with: quiccochet genpsk`)
	w(`psk    = "CHANGE-ME-both-sides-must-match"`)
	w(``)
	w(`# ── Obfuscation ──────────────────────────────────────────────────────`)
	w(`# Padding is applied inside the QUIC datagram, not at the socket, so it`)
	w(`# never defeats GSO. "none" is the fastest and already looks like QUIC.`)
	w(`[obfuscation]`)
	w(`mode      = "%s"        # none | binning | standard | paranoid`, c.Obfs.Mode)
	w(`bin_size  = %d              # round frame length up to a multiple of this`, c.Obfs.BinSize)
	w(`chaff_pps = %d                # cover packets per second; 0 = off`, c.Obfs.ChaffPPS)
	w(`jitter_us = %d                # send-side jitter; costs latency, buy carefully`, c.Obfs.JitterUS)
	w(``)
	w(`# ── Datapath ─────────────────────────────────────────────────────────`)
	w(`[datapath]`)
	w(`workers     = %d               # 0 = one per available core`, c.Datapath.Workers)
	w(`pin_cores   = %t`, c.Datapath.PinCores)
	w(`cores       = []              # explicit affinity list; empty = 0..n-1`)
	w(`coalesce    = %t             # pack several small IP packets per datagram`, c.Datapath.Coalesce)
	w(`coalesce_us = %d              # how long a partial batch may wait`, c.Datapath.CoalesceUS)
	w(`batch_size  = %d`, c.Datapath.BatchSize)
	w(``)
	w(`# ── Management ───────────────────────────────────────────────────────`)
	w(`[admin]`)
	w(`socket = "/run/quiccochet/%s.sock"`, name)
	w(``)
	w(`[metrics]`)
	w(`enabled = %t`, c.Metrics.Enabled)
	w(`listen  = "%s"`, c.Metrics.Listen)
	w(``)
	w(`[log]`)
	w(`level = "%s"          # error | warn | info | debug`, c.Log.Level)

	if opt.Role == RoleServer {
		w(``)
		w(`# ── Optional port forwards into the tunnel ───────────────────────────`)
		w(`# [[forward]]`)
		w(`# ports = ["8080", "8443=80"]`)
		w(`# proto = "both"`)
	}
	return b.String(), nil
}
