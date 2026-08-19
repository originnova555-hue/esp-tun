#!/bin/bash
set -euo pipefail

# spoof-tunnel.sh — QUICochet spoofing tunnel manager
# Manages multiple named tunnels with systemd integration

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY="${SCRIPT_DIR}/quiccochet"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/quiccochet"
STATE_DIR="${XDG_RUNTIME_DIR:-/tmp}/quiccochet"
SYSTEMD_USER_DIR="$HOME/.config/systemd/user"

mkdir -p "$CONFIG_DIR" "$STATE_DIR" "$SYSTEMD_USER_DIR"

usage() {
	cat <<EOF
spoof-tunnel.sh — QUICochet tunnel manager

Usage: spoof-tunnel.sh <command> [options]

Commands:
  list                     List all configured tunnels
  create <name>            Create a new named tunnel (interactive)
  start <name>             Start a tunnel (foreground or systemd)
  stop <name>              Stop a running tunnel
  status <name>            Show tunnel status
  delete <name>            Remove tunnel configuration

  admin <name> <cmd>       Send command to tunnel admin socket
                           Commands: stats, pprof-start, pprof-stop, bench, help
  bench <name> [duration]  Run benchmark on tunnel (default: 10s)
  metrics <name>           Fetch Prometheus metrics from tunnel

  service <name>           Enable/manage systemd service for tunnel
  timer <name> [interval]  Set up recurring restart timer

  wizard                   Interactive wizard to set up a tunnel

Options:
  --help                   Show this message
  --config-dir PATH        Override config directory
  --state-dir PATH         Override state directory
  --systemd                Manage via systemd instead of foreground

Examples:
  spoof-tunnel.sh wizard
  spoof-tunnel.sh create work --tier ultra
  spoof-tunnel.sh start work --systemd
  spoof-tunnel.sh admin iran-main stats
  spoof-tunnel.sh bench iran-main 30
EOF
}

cmd_list() {
	if [[ ! -d "$CONFIG_DIR" ]] || [[ -z "$(ls "$CONFIG_DIR"/*.toml 2>/dev/null)" ]]; then
		echo "No tunnels configured."
		return 0
	fi

	printf "%-20s %-8s %-20s\n" "NAME" "ROLE" "STATUS"
	printf "%-20s %-8s %-20s\n" "---" "---" "---"

	for cfg in "$CONFIG_DIR"/*.toml; do
		name=$(basename "$cfg" .toml)
		role=$(grep "^role" "$cfg" | awk -F'"' '{print $2}' || echo "?")

		if [[ -S "$STATE_DIR/$name.sock" ]]; then
			status="running"
		else
			status="stopped"
		fi

		printf "%-20s %-8s %-20s\n" "$name" "$role" "$status"
	done
}

cmd_create() {
	local name="${1:?missing tunnel name}"
	shift || true

	local tier="ultra"
	local role="client"

	while [[ $# -gt 0 ]]; do
		case "$1" in
			--tier) tier="$2"; shift 2 ;;
			--role) role="$2"; shift 2 ;;
			*) echo "unknown option: $1"; exit 1 ;;
		esac
	done

	local config="$CONFIG_DIR/$name.toml"
	if [[ -f "$config" ]]; then
		echo "Tunnel '$name' already exists at $config"
		return 1
	fi

	"$SCRIPT_DIR/quiccochet" sample --tier "$tier" --role "$role" --name "$name" > "$config"
	echo "Created tunnel configuration at $config"
	echo "Edit manually or use 'wizard' to configure interactively"
}

cmd_wizard() {
	echo "=== QUICochet Tunnel Setup Wizard ==="
	echo

	read -p "Tunnel name (e.g. 'work', 'iran-main'): " name
	if [[ -z "$name" ]]; then
		echo "Aborted."
		return 1
	fi

	if [[ -f "$CONFIG_DIR/$name.toml" ]]; then
		echo "Tunnel '$name' already exists. Exiting."
		return 1
	fi

	echo
	echo "Role:"
	echo "  1. server  - listens for incoming connections"
	echo "  2. client  - connects to a server"
	read -p "Choose [1=server, 2=client]: " role_choice
	role="server"
	[[ "$role_choice" == "2" ]] && role="client"

	echo
	echo "Performance Tier:"
	echo "  1. light   - 1-20 users, minimal resources"
	echo "  2. medium  - 20-100 users, balanced"
	echo "  3. high    - 100-500 users, optimized"
	echo "  4. ultra   - thousands, max throughput"
	read -p "Choose [1-4]: " tier_choice
	case "$tier_choice" in
		1) tier="light" ;;
		2) tier="medium" ;;
		3) tier="high" ;;
		4) tier="ultra" ;;
		*) tier="medium" ;;
	esac

	echo
	read -p "TUN interface name [default: qc0]: " tun_name
	tun_name="${tun_name:-qc0}"

	echo
	read -p "Local CIDR for TUN [default: 10.20.0.1/30]: " tun_local
	tun_local="${tun_local:-10.20.0.1/30}"

	echo
	read -p "Peer IP address [default: 10.20.0.2]: " tun_peer
	tun_peer="${tun_peer:-10.20.0.2}"

	if [[ "$role" == "client" ]]; then
		echo
		read -p "Server address to dial (IP:port): " peer_addr
		if [[ -z "$peer_addr" ]]; then
			echo "Server address required for client mode"
			return 1
		fi
	else
		echo
		read -p "Listen address [default: 0.0.0.0:6262]: " listen_addr
		listen_addr="${listen_addr:-0.0.0.0:6262}"
	fi

	echo
	read -p "Obfuscation mode [none/binning/standard]: " obfuscation
	obfuscation="${obfuscation:-binning}"

	echo
	read -p "Congestion control [cubic/auto]: " congestion
	congestion="${congestion:-cubic}"

	local config="$CONFIG_DIR/$name.toml"
	"$SCRIPT_DIR/quiccochet" sample --tier "$tier" --role "$role" --name "$name" > "$config"

	sed -i "s|^name = \"[^\"]*\"|name = \"$name\"|" "$config"
	sed -i "s|^role = \"[^\"]*\"|role = \"$role\"|" "$config" || true
	sed -i "s|^name    = \"[^\"]*\"|name    = \"$tun_name\"|" "$config" || sed -i "/\[tun\]/a name    = \"$tun_name\"" "$config"
	sed -i "s|^local   = \"[^\"]*\"|local   = \"$tun_local\"|" "$config" || sed -i "/\[tun\]/a local   = \"$tun_local\"" "$config"
	sed -i "s|^peer    = \"[^\"]*\"|peer    = \"$tun_peer\"|" "$config" || sed -i "/\[tun\]/a peer    = \"$tun_peer\"" "$config"
	sed -i "s|^mode      = \"[^\"]*\"|mode      = \"$obfuscation\"|" "$config"
	sed -i "s|^congestion_control    = \"[^\"]*\"|congestion_control    = \"$congestion\"|" "$config"

	echo
	echo "=== Configuration Summary ==="
	echo "Tunnel name: $name"
	echo "Role: $role"
	echo "Tier: $tier"
	echo "TUN: $tun_name ($tun_local peer $tun_peer)"
	echo "Obfuscation: $obfuscation"
	echo "Congestion: $congestion"
	echo
	echo "Configuration saved to $config"
	echo "Start with: spoof-tunnel.sh start $name"
}

cmd_start() {
	local name="${1:?missing tunnel name}"
	shift || true

	local config="$CONFIG_DIR/$name.toml"
	if [[ ! -f "$config" ]]; then
		echo "Tunnel '$name' not found at $config"
		return 1
	fi

	local use_systemd=0
	while [[ $# -gt 0 ]]; do
		case "$1" in
			--systemd) use_systemd=1; shift ;;
			*) echo "unknown option: $1"; exit 1 ;;
		esac
	done

	if [[ $use_systemd -eq 1 ]]; then
		cmd_service "$name" enable
		systemctl --user start "quiccochet-$name.service"
		echo "Started tunnel '$name' via systemd"
	else
		echo "Starting tunnel '$name' in foreground..."
		"$BINARY" run -config "$config"
	fi
}

cmd_stop() {
	local name="${1:?missing tunnel name}"

	if systemctl --user is-active --quiet "quiccochet-$name.service" 2>/dev/null; then
		systemctl --user stop "quiccochet-$name.service"
		echo "Stopped tunnel '$name'"
	else
		echo "Tunnel '$name' is not running"
	fi
}

cmd_status() {
	local name="${1:?missing tunnel name}"

	if systemctl --user is-active --quiet "quiccochet-$name.service" 2>/dev/null; then
		echo "Status: running (systemd)"
		systemctl --user status "quiccochet-$name.service"
	elif [[ -S "$STATE_DIR/$name.sock" ]]; then
		echo "Status: running (foreground)"
	else
		echo "Status: stopped"
	fi
}

cmd_admin() {
	local name="${1:?missing tunnel name}"
	local cmd="${2:?missing admin command}"
	shift 2 || true

	local socket="$STATE_DIR/$name.sock"
	if [[ ! -S "$socket" ]]; then
		echo "Tunnel '$name' is not running (no admin socket)"
		return 1
	fi

	echo "$cmd $@" | nc -U "$socket" || echo "Admin command failed"
}

cmd_bench() {
	local name="${1:?missing tunnel name}"
	local duration="${2:-10}"

	cmd_admin "$name" bench "$duration"
}

cmd_metrics() {
	local name="${1:?missing tunnel name}"
	local metrics_port

	metrics_port=$(grep "^listen.*= \"" "$CONFIG_DIR/$name.toml" 2>/dev/null | grep metrics | awk -F':' '{print $NF}' | tr -d '"' || echo "9808")

	echo "Fetching metrics from 127.0.0.1:$metrics_port/metrics..."
	curl -s "http://127.0.0.1:$metrics_port/metrics" || echo "Failed to fetch metrics"
}

cmd_service() {
	local name="${1:?missing tunnel name}"
	local action="${2:-status}"

	local config="$CONFIG_DIR/$name.toml"
	if [[ ! -f "$config" ]]; then
		echo "Tunnel '$name' not found"
		return 1
	fi

	local service_file="$SYSTEMD_USER_DIR/quiccochet-$name.service"
	local timer_file="$SYSTEMD_USER_DIR/quiccochet-$name.timer"

	case "$action" in
		enable)
			cat > "$service_file" <<EOF
[Unit]
Description=QUICochet Tunnel: $name
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BINARY run -config $config
Restart=on-failure
RestartSec=5

User=$(whoami)
Environment="PATH=$PATH"

StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
EOF
			systemctl --user daemon-reload
			systemctl --user enable "quiccochet-$name.service"
			echo "Service enabled: quiccochet-$name"
			;;
		disable)
			systemctl --user disable "quiccochet-$name.service" 2>/dev/null || true
			rm -f "$service_file"
			systemctl --user daemon-reload
			echo "Service disabled: quiccochet-$name"
			;;
		status)
			if [[ -f "$service_file" ]]; then
				systemctl --user status "quiccochet-$name.service"
			else
				echo "Service not configured"
			fi
			;;
		*)
			echo "Unknown service action: $action (use: enable, disable, status)"
			;;
	esac
}

cmd_timer() {
	local name="${1:?missing tunnel name}"
	local interval="${2:-1h}"

	local service_file="$SYSTEMD_USER_DIR/quiccochet-$name.service"
	local timer_file="$SYSTEMD_USER_DIR/quiccochet-$name.timer"

	if [[ ! -f "$service_file" ]]; then
		echo "Service must be configured first: spoof-tunnel.sh service $name enable"
		return 1
	fi

	cat > "$timer_file" <<EOF
[Unit]
Description=Restart QUICochet Tunnel: $name
Requires=quiccochet-$name.service

[Timer]
OnBootSec=5min
OnUnitActiveSec=$interval
Unit=quiccochet-$name.service

[Install]
WantedBy=timers.target
EOF

	systemctl --user daemon-reload
	systemctl --user enable "quiccochet-$name.timer"
	systemctl --user start "quiccochet-$name.timer"
	echo "Timer enabled for '$name' (restarts every $interval)"
}

cmd_delete() {
	local name="${1:?missing tunnel name}"

	cmd_stop "$name" || true

	local config="$CONFIG_DIR/$name.toml"
	if [[ -f "$config" ]]; then
		rm "$config"
		echo "Deleted configuration: $config"
	fi

	cmd_service "$name" disable || true

	if [[ -f "$SYSTEMD_USER_DIR/quiccochet-$name.timer" ]]; then
		systemctl --user disable "quiccochet-$name.timer" 2>/dev/null || true
		rm "$SYSTEMD_USER_DIR/quiccochet-$name.timer"
	fi

	echo "Tunnel '$name' deleted"
}

main() {
	local cmd="${1:-}"
	shift || true

	case "$cmd" in
		list)          cmd_list "$@" ;;
		create)        cmd_create "$@" ;;
		start)         cmd_start "$@" ;;
		stop)          cmd_stop "$@" ;;
		status)        cmd_status "$@" ;;
		delete)        cmd_delete "$@" ;;
		admin)         cmd_admin "$@" ;;
		bench)         cmd_bench "$@" ;;
		metrics)       cmd_metrics "$@" ;;
		service)       cmd_service "$@" ;;
		timer)         cmd_timer "$@" ;;
		wizard)        cmd_wizard "$@" ;;
		--help|-h|"")  usage; exit 0 ;;
		*)             echo "Unknown command: $cmd"; usage; exit 1 ;;
	esac
}

main "$@"
