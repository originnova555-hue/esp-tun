// Package config defines the on-disk configuration schema for the tunnel and
// the tier presets that sit on top of it.
//
// A tier is deliberately nothing more than a set of defaults over the normal
// config surface: it is applied first, then the operator's TOML is decoded on
// top, so any key written by hand always wins over the preset.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Role decides which side dials and which side listens.
type Role string

const (
	RoleServer Role = "server" // listens; typically the Iran-side entry point
	RoleClient Role = "client" // dials; typically the foreign exit
)

// Config is the whole tunnel instance configuration.
type Config struct {
	Name string `toml:"name"`
	Role Role   `toml:"role"`
	Tier string `toml:"tier"`

	TUN       TUN       `toml:"tun"`
	Transport Transport `toml:"transport"`
	QUIC      QUIC      `toml:"quic"`
	Crypto    Crypto    `toml:"crypto"`
	Obfs      Obfs      `toml:"obfuscation"`
	Datapath  Datapath  `toml:"datapath"`
	Admin     Admin     `toml:"admin"`
	Metrics   Metrics   `toml:"metrics"`
	Log       Log       `toml:"log"`

	Forwards []Forward `toml:"forward"`
}

// TUN describes the persistent layer-3 interface. Its lifetime is deliberately
// decoupled from the session lifetime: it is created once at startup and is
// never torn down on reconnect.
type TUN struct {
	Name    string `toml:"name"`
	Local   string `toml:"local"` // CIDR, e.g. "10.20.0.1/30"
	Peer    string `toml:"peer"`  // bare address, e.g. "10.20.0.2"
	MTU     int    `toml:"mtu"`
	Queues  int    `toml:"queues"` // multiqueue fds; 0 = derive from datapath.workers
	Persist bool   `toml:"persist"`
}

// Transport is the spoofed UDP socket underneath QUIC.
type Transport struct {
	Listen      string   `toml:"listen"`       // local bind, e.g. "0.0.0.0:6262"
	Peer        string   `toml:"peer"`         // remote real address (client dials this)
	SpoofSrc    []string `toml:"spoof_src"`    // whitelisted addresses we send *from*
	SpoofExpect []string `toml:"spoof_expect"` // whitelisted addresses we expect to receive *from*
	Transparent bool     `toml:"transparent"`  // IP_TRANSPARENT; required for a non-local source

	RcvBufferMB  int  `toml:"rcv_buffer_mb"`
	SndBufferMB  int  `toml:"snd_buffer_mb"`
	ForceBuffers bool `toml:"force_buffers"` // try SO_RCVBUFFORCE / SO_SNDBUFFORCE first

	GSO bool `toml:"gso"`
	GRO bool `toml:"gro"`

	Health Health `toml:"health"`
}

// Health drives the spoof-source probing, quarantine and rotation loop.
type Health struct {
	Enabled       bool `toml:"enabled"`
	IntervalSec   int  `toml:"interval_sec"`
	TimeoutSec    int  `toml:"timeout_sec"`
	FailThreshold int  `toml:"fail_threshold"`
	QuarantineSec int  `toml:"quarantine_sec"`
}

// QUIC configures the session pool. Only DATAGRAM frames (RFC 9221) carry
// tunnel payload; no streams are used for the datapath, so a lost packet is
// never retransmitted underneath the inner flow's own recovery.
type QUIC struct {
	PoolSize            int    `toml:"pool_size"`
	KeepAlivePeriodSec  int    `toml:"keep_alive_period_sec"`
	MaxIdleTimeoutSec   int    `toml:"max_idle_timeout_sec"`
	HandshakeTimeoutSec int    `toml:"handshake_timeout_sec"`
	InitialPacketSize   int    `toml:"initial_packet_size"`
	DisablePMTUD        bool   `toml:"disable_pmtud"`
	CongestionControl   string `toml:"congestion_control"`
	DatagramQueue       int    `toml:"datagram_queue"`

	ReconnectMinMs int `toml:"reconnect_min_ms"`
	ReconnectMaxMs int `toml:"reconnect_max_ms"`
}

// Crypto selects the AEAD and carries the pre-shared key that replaces a PKI.
type Crypto struct {
	Cipher string `toml:"cipher"` // auto | aes-256-gcm | chacha20-poly1305
	PSK    string `toml:"psk"`
}

// Obfs is the tiered traffic-shaping layer. Padding happens inside the QUIC
// datagram, never at the socket, so it never defeats GSO.
type Obfs struct {
	Mode     string `toml:"mode"` // none | binning | standard | paranoid
	BinSize  int    `toml:"bin_size"`
	ChaffPPS int    `toml:"chaff_pps"`
	JitterUS int    `toml:"jitter_us"`
}

// Datapath tunes the TUN <-> QUIC pump.
type Datapath struct {
	Workers    int   `toml:"workers"` // 0 = one per available core
	PinCores   bool  `toml:"pin_cores"`
	Cores      []int `toml:"cores"`
	Coalesce   bool  `toml:"coalesce"`
	CoalesceUS int   `toml:"coalesce_us"`
	BatchSize  int   `toml:"batch_size"`
}

// Admin is the local control socket (stats, pprof, bench).
type Admin struct {
	Socket string `toml:"socket"`
}

// Metrics is the Prometheus exporter.
type Metrics struct {
	Enabled bool   `toml:"enabled"`
	Listen  string `toml:"listen"`
}

// Log controls verbosity.
type Log struct {
	Level string `toml:"level"` // error | warn | info | debug
}

// Forward is an optional server-side port forward into the tunnel.
type Forward struct {
	Ports []string `toml:"ports"` // "8080" or "8443=80"
	Proto string   `toml:"proto"` // tcp | udp | both
}

// Durations -------------------------------------------------------------

func (h Health) Interval() time.Duration   { return time.Duration(h.IntervalSec) * time.Second }
func (h Health) Timeout() time.Duration    { return time.Duration(h.TimeoutSec) * time.Second }
func (h Health) Quarantine() time.Duration { return time.Duration(h.QuarantineSec) * time.Second }

func (q QUIC) KeepAlive() time.Duration { return time.Duration(q.KeepAlivePeriodSec) * time.Second }
func (q QUIC) MaxIdle() time.Duration   { return time.Duration(q.MaxIdleTimeoutSec) * time.Second }
func (q QUIC) Handshake() time.Duration { return time.Duration(q.HandshakeTimeoutSec) * time.Second }
func (q QUIC) ReconnectMin() time.Duration {
	return time.Duration(q.ReconnectMinMs) * time.Millisecond
}
func (q QUIC) ReconnectMax() time.Duration {
	return time.Duration(q.ReconnectMaxMs) * time.Millisecond
}

func (o Obfs) Jitter() time.Duration { return time.Duration(o.JitterUS) * time.Microsecond }
func (d Datapath) CoalesceWindow() time.Duration {
	return time.Duration(d.CoalesceUS) * time.Microsecond
}

// Load reads a config file, applies the tier preset named inside it, then
// decodes the file on top so hand-written keys always win.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse does the two-pass tier overlay on an in-memory document.
func Parse(raw []byte) (*Config, error) {
	// Pass 1: we only care which tier was requested.
	var probe struct {
		Tier string `toml:"tier"`
		Role Role   `toml:"role"`
	}
	if _, err := toml.Decode(string(raw), &probe); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	tier := probe.Tier
	if tier == "" {
		tier = TierMedium
	}
	cfg, err := Preset(tier)
	if err != nil {
		return nil, err
	}

	// Pass 2: the operator's keys overwrite the preset in place.
	if _, err := toml.Decode(string(raw), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.Tier = tier
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) normalize() {
	c.Role = Role(strings.ToLower(string(c.Role)))
	c.Crypto.Cipher = strings.ToLower(strings.TrimSpace(c.Crypto.Cipher))
	c.Obfs.Mode = strings.ToLower(strings.TrimSpace(c.Obfs.Mode))
	c.QUIC.CongestionControl = strings.ToLower(strings.TrimSpace(c.QUIC.CongestionControl))
	c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))

	if c.TUN.Queues <= 0 {
		c.TUN.Queues = c.Datapath.Workers
	}
	if c.Admin.Socket == "" && c.Name != "" {
		c.Admin.Socket = "/run/quiccochet/" + c.Name + ".sock"
	}
}

// LocalPrefix returns the TUN address as a prefix.
func (t TUN) LocalPrefix() (netip.Prefix, error) { return netip.ParsePrefix(t.Local) }

// PeerAddr returns the TUN peer address.
func (t TUN) PeerAddr() (netip.Addr, error) { return netip.ParseAddr(t.Peer) }
