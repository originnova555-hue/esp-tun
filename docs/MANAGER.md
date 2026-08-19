# spoof-tunnel.sh — QUICochet Tunnel Manager

Multi-tunnel orchestration script for QUICochet spoofing tunnels with systemd integration.

## Quick Start

```bash
# Interactive setup wizard
./scripts/spoof-tunnel.sh wizard

# Or create a tunnel directly
./scripts/spoof-tunnel.sh create iran-main --tier ultra

# Start in foreground
./scripts/spoof-tunnel.sh start iran-main

# Start as systemd service
./scripts/spoof-tunnel.sh start iran-main --systemd
```

## Features

- **Named Tunnels**: Manage multiple independent tunnels by name
- **Interactive Setup**: Wizard guides you through configuration
- **Systemd Integration**: Persistent service management with timers
- **Admin Socket**: Query stats, run benchmarks, manage profiling
- **Metrics Export**: Prometheus metrics on HTTP endpoint
- **Multi-Tunnel Isolation**: Each tunnel has independent config, state, and systemd service

## Commands

### Management

```bash
# List all configured tunnels
spoof-tunnel.sh list

# Create new tunnel (interactive or with options)
spoof-tunnel.sh create work --tier medium --role client
spoof-tunnel.sh wizard

# Start/stop tunnels
spoof-tunnel.sh start iran-main
spoof-tunnel.sh start iran-main --systemd    # Use systemd
spoof-tunnel.sh stop iran-main

# Check status
spoof-tunnel.sh status iran-main

# Delete tunnel and configs
spoof-tunnel.sh delete iran-main
```

### Admin & Monitoring

```bash
# Send commands to tunnel admin socket
spoof-tunnel.sh admin iran-main stats         # Pool stats
spoof-tunnel.sh admin iran-main help          # Show commands
spoof-tunnel.sh admin iran-main pprof-start   # Start CPU profile
spoof-tunnel.sh admin iran-main pprof-stop    # Stop and save profile

# Run benchmark
spoof-tunnel.sh bench iran-main 30            # 30-second benchmark

# Fetch Prometheus metrics
spoof-tunnel.sh metrics iran-main
```

### Systemd Management

```bash
# Enable systemd service (creates .service file)
spoof-tunnel.sh service iran-main enable

# Set up auto-restart timer
spoof-tunnel.sh timer iran-main 1h            # Restart every hour

# Show service status
spoof-tunnel.sh service iran-main status

# Disable service
spoof-tunnel.sh service iran-main disable
```

## Configuration

Tunnel configurations are stored as TOML files in `$XDG_CONFIG_HOME/quiccochet/` (default: `~/.config/quiccochet/`).

### Example Configuration

```toml
name = "iran-main"
role = "server"
tier = "ultra"

[tun]
name    = "qc0"
local   = "10.20.0.1/30"
peer    = "10.20.0.2"
mtu     = 1360
persist = true

[transport]
listen         = "0.0.0.0:6262"
spoof_src      = ["62.60.212.216"]
spoof_expect   = ["5.34.222.2"]
transparent    = true
rcv_buffer_mb  = 32
snd_buffer_mb  = 32

[quic]
pool_size             = 16
keep_alive_period_sec = 5
max_idle_timeout_sec  = 30
handshake_timeout_sec = 10
datagram_queue        = 1024
congestion_control    = "cubic"

[obfuscation]
mode      = "binning"
bin_size  = 512
chaff_pps = 0
jitter_us = 0

[metrics]
enabled = true
listen  = "127.0.0.1:9808"
```

## Admin Socket Protocol

The admin socket accepts newline-terminated commands:

```bash
# Connect to admin socket
nc -U /run/quiccochet/iran-main.sock

# Send commands
stats
help
pprof-start cpu
pprof-stop
bench 10
```

### Commands

- `stats` — Show connection pool statistics
- `help` — List available commands
- `pprof-start [cpu|mem]` — Start profiling (default: CPU)
- `pprof-stop` — Stop active profile, write to `/tmp/quiccochet.prof`
- `bench [duration]` — Run throughput benchmark for N seconds (default: 10)

## Systemd Integration

Each tunnel gets its own systemd service and optional timer:

```bash
~/.config/systemd/user/quiccochet-iran-main.service
~/.config/systemd/user/quiccochet-iran-main.timer
```

Reload and manage with:

```bash
systemctl --user daemon-reload
systemctl --user start quiccochet-iran-main.service
systemctl --user enable quiccochet-iran-main.timer
journalctl --user -u quiccochet-iran-main -f
```

## Examples

### Set up a production server tunnel

```bash
./scripts/spoof-tunnel.sh wizard

# Follow prompts:
# - Name: iran-main
# - Role: server
# - Tier: ultra
# - Obfuscation: binning
# - Congestion: cubic

./scripts/spoof-tunnel.sh service iran-main enable
./scripts/spoof-tunnel.sh start iran-main --systemd
./scripts/spoof-tunnel.sh status iran-main
```

### Monitor an active tunnel

```bash
# Check stats every 5 seconds
while true; do
  echo "=== $(date) ==="
  ./scripts/spoof-tunnel.sh admin iran-main stats
  sleep 5
done
```

### Run benchmark and collect profile

```bash
# Start tunnel in background
./scripts/spoof-tunnel.sh start work --systemd &

# Let it stabilize
sleep 2

# Run benchmark
./scripts/spoof-tunnel.sh bench work 30

# Get stats
./scripts/spoof-tunnel.sh admin work stats

# Fetch metrics
./scripts/spoof-tunnel.sh metrics work
```

### Set up auto-restart

```bash
# Enable service
./scripts/spoof-tunnel.sh service iran-main enable

# Auto-restart every hour
./scripts/spoof-tunnel.sh timer iran-main 1h

# Check timer
systemctl --user list-timers quiccochet-*
```

### Multi-tunnel orchestration

```bash
# Create three independent tunnels
for name in iran-main asia-edge eu-west; do
  ./scripts/spoof-tunnel.sh create $name --tier high
done

# Start all as services
for name in iran-main asia-edge eu-west; do
  ./scripts/spoof-tunnel.sh service $name enable
  ./scripts/spoof-tunnel.sh start $name --systemd
done

# Check status
./scripts/spoof-tunnel.sh list

# Individually control
./scripts/spoof-tunnel.sh admin iran-main stats
./scripts/spoof-tunnel.sh admin asia-edge stats
./scripts/spoof-tunnel.sh admin eu-west stats
```

## State and Logs

- **Configs**: `$XDG_CONFIG_HOME/quiccochet/*.toml`
- **State**: `$XDG_RUNTIME_DIR/quiccochet/*.sock`
- **Profiles**: `/tmp/quiccochet.prof`
- **Logs** (systemd): `journalctl --user -u quiccochet-*`

## Troubleshooting

### Tunnel won't start

```bash
# Check systemd journal
journalctl --user -u quiccochet-iran-main -n 50 -e

# Validate config
./quiccochet check -config ~/.config/quiccochet/iran-main.toml

# Try foreground to see errors
./scripts/spoof-tunnel.sh start iran-main
```

### Admin socket not responding

```bash
# Check if tunnel is running
./scripts/spoof-tunnel.sh status iran-main

# Verify socket exists
ls -la /run/quiccochet/iran-main.sock

# Reconnect and try again
sleep 2
./scripts/spoof-tunnel.sh admin iran-main stats
```

### Permission denied for IP_TRANSPARENT

Ensure you're running with sufficient capabilities or as root:

```bash
# Option 1: Run as root or with sudo
sudo ./scripts/spoof-tunnel.sh start iran-main

# Option 2: Grant capability to binary
sudo setcap cap_net_admin=ep ./quiccochet

# Option 3: Run under systemd with appropriate settings
```

## File Locations

```
~/.config/quiccochet/                    # Tunnel configs
  ├── iran-main.toml
  ├── work.toml
  └── ...

$XDG_RUNTIME_DIR/quiccochet/             # Runtime state
  ├── iran-main.sock                     # Admin socket
  └── ...

~/.config/systemd/user/                  # Systemd units
  ├── quiccochet-iran-main.service
  ├── quiccochet-iran-main.timer
  └── ...

/tmp/quiccochet.prof                     # CPU profile output
```
