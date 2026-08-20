#!/usr/bin/env bash
# =============================================================================
# spoof-tunnel.sh — QUICochet Tunnel manager
#
# Refactored for the QUICochet TUN/L3 engine (JSON config, tun.*,
# peers[], four performance tiers) — same menu/service/timer UX as the
# original spoof-tunnel.sh, retargeted at the new binary and schema.
# =============================================================================
set -euo pipefail

BINARY_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$BINARY_DIR/quiccochet"
TIERS_DIR="$BINARY_DIR/tiers"
CONFIG_DIR="/etc/spoof-tunnel"
SVC_PREFIX="spoof-tunnel"

# Known whitelisted IPs (defaults) — same pair as before, still the
# common Iran<->foreign case.
DEFAULT_IRAN_SPOOF_SRC="62.60.212.216"    # Iran -> sends with this src
DEFAULT_IRAN_SPOOF_DST="5.34.222.2"       # Iran -> expects this incoming src
DEFAULT_FOREIGN_SPOOF_SRC="5.34.222.2"    # Foreign -> sends with this src
DEFAULT_FOREIGN_SPOOF_DST="62.60.212.216" # Foreign -> expects this incoming src

# ── Colors ────────────────────────────────────────────────────────────────────
R=$'\e[0;31m' G=$'\e[0;32m' Y=$'\e[1;33m' C=$'\e[0;36m'
W=$'\e[1;37m' DIM=$'\e[2m' BOLD=$'\e[1m' RST=$'\e[0m'

p()   { printf '%s\n' "$*"; }
pi()  { printf "${C}  ▶ %s${RST}\n" "$*"; }
pok() { printf "${G}  ✔ %s${RST}\n" "$*"; }
pw()  { printf "${Y}  ⚠ %s${RST}\n" "$*"; }
pe()  { printf "${R}  ✘ %s${RST}\n" "$*" >&2; }
die() { pe "$*"; exit 1; }
br()  { p ""; }
sep() { p  "  ──────────────────────────────────────"; }

require_root()   { [[ $EUID -eq 0 ]] || die "Run as root (sudo ./spoof-tunnel.sh)"; }
require_binary() { [[ -x "$BINARY" ]] || die "Binary not found: $BINARY"; }
require_python() { command -v python3 >/dev/null 2>&1 || die "python3 is required (used to read/write JSON configs)"; }

svc_name() { printf '%s' "${SVC_PREFIX}-${1}"; }
cfg_path() { printf '%s' "${CONFIG_DIR}/${1}.json"; }

list_tunnels() {
    mkdir -p "$CONFIG_DIR"
    find "$CONFIG_DIR" -maxdepth 1 -name "*.json" 2>/dev/null \
        | sed 's|.*/||;s|\.json$||' | sort
}

# tun_name_owner <iface> [skip_tunnel] — prints the tunnel name that
# already has tun.name == <iface> in its config, empty if none. Used to
# stop two tunnels on the same box from fighting over one TUN device
# (each is a separate process opening/attaching to it independently).
tun_name_owner() {
    local iface="$1" skip="${2:-}" t cfg other
    for t in $(list_tunnels); do
        [[ "$t" == "$skip" ]] && continue
        cfg="$(cfg_path "$t")"
        other="$(json_get "$cfg" "tun.name" "")"
        if [[ "$other" == "$iface" ]]; then
            printf '%s' "$t"
            return 0
        fi
    done
    return 1
}

# next_free_tun_name — first qc<N> (N=0,1,2,...) not already claimed by
# an existing tunnel's tun.name, for use as this new tunnel's default.
next_free_tun_name() {
    # Pre-increment: with n starting at 0, `((n++))` evaluates to the
    # *old* value (0) on the first taken name, whose arithmetic truth
    # is false — under `set -e` that would kill the whole script right
    # here. `((++n))` evaluates to the new (post-increment) value instead,
    # which is never zero.
    local n=0
    while tun_name_owner "qc${n}" >/dev/null; do
        ((++n))
    done
    printf 'qc%d' "$n"
}

svc_state() {
    systemctl is-active "$(svc_name "$1")" 2>/dev/null || printf 'inactive'
}

timer_name() { printf '%s-restart.timer' "$(svc_name "$1")"; }
restart_unit_name() { printf '%s-restart.service' "$(svc_name "$1")"; }

timer_state() {
    systemctl is-active "$(timer_name "$1")" 2>/dev/null || printf 'inactive'
}

normalize_restart_minutes() {
    # Accepts only plain minutes: 1, 5, 30, 120. 0/off disables the timer.
    local v="${1// /}"
    [[ -n "$v" ]] || return 1
    case "${v,,}" in
        off|disable|disabled|0) printf 'off'; return 0 ;;
    esac
    [[ "$v" =~ ^[0-9]+$ ]] || return 1
    (( v > 0 )) || return 1
    printf '%s' "$v"
}

restart_timer_minutes() {
    local name="$1"
    local timer_file="/etc/systemd/system/$(timer_name "$name")"
    [[ -f "$timer_file" ]] || { printf 'off'; return 0; }
    local v
    v="$(grep -E '^OnUnitActiveSec=' "$timer_file" 2>/dev/null | head -1 | cut -d= -f2-)"
    v="${v// /}"
    if [[ "$v" =~ ^([0-9]+)min$ ]]; then
        printf '%s' "${BASH_REMATCH[1]}"
    elif [[ "$v" =~ ^([0-9]+)$ ]]; then
        printf '%ss' "${BASH_REMATCH[1]}"
    else
        printf '%s' "${v:-off}"
    fi
}

manage_restart_timer_menu() {
    local name="$1"
    while true; do
        header
        local svc; svc="$(svc_name "$name")"
        local tst; tst="$(timer_state "$name")"
        local cur; cur="$(restart_timer_minutes "$name")"
        local tc=$R; [[ "$tst" == "active" ]] && tc=$G

        printf "  ${BOLD}Tunnel : ${W}%s${RST}\n" "$name"
        printf "  ${BOLD}Service: ${W}%s.service${RST}\n" "$svc"
        printf "  Timer  : ${tc}${BOLD}%s${RST}\n" "$tst"
        if [[ "$cur" == "off" ]]; then
            printf "  Every  : ${DIM}disabled${RST}\n"
        else
            printf "  Every  : ${W}%s minute(s)${RST}\n" "$cur"
        fi
        br
        sep
        printf "  ${BOLD}%s${RST}  %s\n" "1)" "Set / edit restart timer ${DIM}(minutes)${RST}"
        printf "  ${BOLD}%s${RST}  %s\n" "2)" "Disable restart timer"
        printf "  ${BOLD}%s${RST}  %s\n" "3)" "Back"
        sep
        br
        printf "  Select: "
        local ch; read -r ch
        case "$ch" in
            1) require_root
               br
               p "  ${BOLD}Restart timer in minutes${RST}"
               p "  Example: enter ${W}5${RST} to restart every 5 minutes, ${W}60${RST} for hourly."
               p "  Enter ${W}0${RST} or ${W}off${RST} to disable."
               br
               if [[ "$cur" != "off" ]]; then
                   printf "  Minutes [current: %s]: " "$cur"
               else
                   printf "  Minutes: "
               fi
               local raw minutes
               read -r raw
               [[ -z "$raw" && "$cur" != "off" ]] && raw="$cur"
               minutes="$(normalize_restart_minutes "$raw")" || { pe "Invalid value. Enter minutes only, like 5 or 30."; pause; continue; }
               if [[ "$minutes" == "off" ]]; then
                   _disable_restart_timer "$name"
               else
                   _install_restart_timer "$name" "$minutes"
               fi
               pause ;;
            2) require_root; _disable_restart_timer "$name"; pause ;;
            3|b|B|q|Q) return ;;
            *) pw "Invalid"; pause ;;
        esac
    done
}

local_ip() {
    # No default route (isolated netns, minimal box, IPv6-only egress,
    # etc.) makes `ip route get` fail; under set -o pipefail that would
    # otherwise abort the whole wizard instead of falling back to manual
    # entry, so swallow the failure and return empty.
    { ip -4 route get 8.8.8.8 2>/dev/null \
        | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}' | head -1; } || true
}

# suggested_tier — picks a default tier (1=light .. 4=ultra) from this
# box's real core count / RAM, so a weak VPS defaults toward light/medium
# and a strong or dedicated box defaults toward high/ultra instead of
# everyone landing on the same guess. Whichever signal is weaker wins
# (a low-RAM/high-core or high-RAM/low-core box is still a weak box for
# this purpose) — this is only ever a starting suggestion, the operator
# picks the number and can override it.
suggested_tier() {
    local cores mem_kb mem_gb by_cores by_mem
    cores="$(nproc 2>/dev/null || echo 1)"
    mem_kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null)"
    mem_gb=$(( ${mem_kb:-0} / 1024 / 1024 ))

    if   (( cores >= 8 )); then by_cores=4
    elif (( cores >= 4 )); then by_cores=3
    elif (( cores >= 2 )); then by_cores=2
    else                        by_cores=1
    fi
    if   (( mem_gb >= 8 )); then by_mem=4
    elif (( mem_gb >= 4 )); then by_mem=3
    elif (( mem_gb >= 2 )); then by_mem=2
    else                         by_mem=1
    fi

    (( by_cores < by_mem )) && printf '%s' "$by_cores" || printf '%s' "$by_mem"
}

clr()   { printf '\033[2J\033[H'; }
pause() { br; printf "  ${DIM}[Enter to continue]${RST}"; read -r; }

# ── JSON helpers (python3-backed; no jq dependency assumed) ─────────────────
# json_get <file> <dotted.path> [default] — dotted path may index arrays
# with a numeric segment, e.g. "peers.0.name".
json_get() {
    python3 - "$1" "$2" "${3:-}" <<'PYEOF'
import json, sys
path, key, default = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    with open(path) as f:
        d = json.load(f)
    for seg in key.split("."):
        d = d[int(seg)] if isinstance(d, list) else d[seg]
    print(d if not isinstance(d, (dict, list)) else json.dumps(d))
except Exception:
    print(default)
PYEOF
}

# ── Header ────────────────────────────────────────────────────────────────────
header() {
    clr
    printf "${W}${BOLD}"
    p  "  ╔══════════════════════════════════════════╗"
    p  "  ║         QUICochet Tunnel  Manager        ║"
    p  "  ╚══════════════════════════════════════════╝"
    printf "${RST}"
    br
}

# ── Main menu ─────────────────────────────────────────────────────────────────
main_menu() {
    while true; do
        header

        sep
        printf "  ${BOLD}%s${RST}  %s\n" "1)" "Create Iran tunnel    ${DIM}(this machine = server)${RST}"
        printf "  ${BOLD}%s${RST}  %s\n" "2)" "Create Foreign tunnel ${DIM}(this machine = client)${RST}"
        sep

        mapfile -t tunnels < <(list_tunnels)
        local n=${#tunnels[@]}

        if [[ $n -gt 0 ]]; then
            br
            printf "  ${BOLD}${C}%-4s %-24s %s${RST}\n" "No." "Tunnel" "Status"
            sep
            local i=3
            for t in "${tunnels[@]}"; do
                local st; st="$(svc_state "$t")"
                local sc=$R; [[ "$st" == "active" ]] && sc=$G
                printf "  ${BOLD}%-4s${RST} %-24s ${sc}${BOLD}%s${RST}\n" "${i})" "$t" "$st"
                ((i++))
            done
        fi

        br
        sep
        printf "  ${BOLD}%s${RST}  %s\n" "q)" "Quit"
        sep
        br
        printf "  Select: "
        local ch; read -r ch

        case "$ch" in
            1) tunnel_new_wizard "server" ;;
            2) tunnel_new_wizard "client" ;;
            q|Q) clr; exit 0 ;;
            "")  ;;
            *)
                if [[ "$ch" =~ ^[0-9]+$ ]] && (( ch >= 3 && ch <= n+2 )); then
                    tunnel_manage_menu "${tunnels[$((ch-3))]}"
                else
                    pw "Invalid choice"; pause
                fi ;;
        esac
    done
}

# ── Tunnel manage submenu ─────────────────────────────────────────────────────
tunnel_manage_menu() {
    local name="$1"
    local svc; svc="$(svc_name "$name")"
    while true; do
        header
        local st; st="$(svc_state "$name")"
        local sc=$R; [[ "$st" == "active" ]] && sc=$G
        local cfg; cfg="$(cfg_path "$name")"
        local mode; mode="$(json_get "$cfg" "mode" "?")"
        local tier; tier="$(json_get "$cfg" "_tier_name" "custom")"

        printf "  ${BOLD}Tunnel : ${W}%s${RST}  ${DIM}(%s, tier: %s)${RST}\n" "$name" "$mode" "$tier"
        printf "  Status : ${sc}${BOLD}%s${RST}\n" "$st"
        printf "  Config : ${DIM}%s${RST}\n" "$cfg"
        br
        sep
        printf "  ${BOLD}%s${RST}  %s\n" "1)" "Start"
        printf "  ${BOLD}%s${RST}  %s\n" "2)" "Stop"
        printf "  ${BOLD}%s${RST}  %s\n" "3)" "Restart"
        printf "  ${BOLD}%s${RST}  %s\n" "4)" "View logs (live)"
        printf "  ${BOLD}%s${RST}  %s\n" "5)" "Show config"
        printf "  ${BOLD}%s${RST}  %s\n" "6)" "Edit config"
        printf "  ${BOLD}%s${RST}  %s\n" "7)" "Reinstall service"
        printf "  ${BOLD}%s${RST}  %s\n" "8)" "Auto restart timer ${DIM}(edit minutes)${RST}"
        printf "  ${BOLD}%s${RST}  %s\n" "9)" "Live stats ${DIM}(admin socket)${RST}"
        printf "  ${BOLD}%s${RST}  %s\n" "10)" "Benchmark ${DIM}(throughput/latency over live tunnel)${RST}"
        printf "  ${BOLD}%s${RST}  %s\n" "11)" "Remove tunnel"
        printf "  ${BOLD}%s${RST}  %s\n" "12)" "Back"
        sep
        br
        printf "  Select: "
        local ch; read -r ch

        case "$ch" in
            1) require_root
               systemctl start "$svc" && pok "Started: $svc" || pe "Failed"; pause ;;
            2) require_root
               systemctl stop  "$svc" && pok "Stopped: $svc" || pe "Failed"; pause ;;
            3) require_root
               systemctl restart "$svc" && pok "Restarted: $svc" || pe "Failed"; pause ;;
            4) journalctl -u "$svc" -f --no-pager ;;
            5) clr; cat "$cfg"; pause ;;
            6) ${EDITOR:-nano} "$cfg" ;;
            7) require_root; _install_svc "$name"; pause ;;
            8) manage_restart_timer_menu "$name" ;;
            9) _admin_stats "$name"; pause ;;
            10) _admin_bench_menu "$name" ;;
            11) require_root
               br; printf "  ${Y}Remove tunnel '${name}'? [y/N]:${RST} "; read -r ans
               if [[ "${ans,,}" == "y" ]]; then
                   _remove_tunnel "$name"
                   pok "Removed: $name"; pause; return
               fi ;;
            12|b|B|q|Q) return ;;
            *) pw "Invalid"; pause ;;
        esac
    done
}

_admin_socket_for() {
    local cfg; cfg="$(cfg_path "$1")"
    json_get "$cfg" "admin.socket" ""
}

_admin_stats() {
    local name="$1"
    local sock; sock="$(_admin_socket_for "$name")"
    [[ -n "$sock" && -S "$sock" ]] || { pw "Admin socket not available (is the tunnel running?)"; return; }
    br
    "$BINARY" admin --socket "$sock" -H stats 2>&1 || pe "admin stats failed"
}

_admin_bench_menu() {
    local name="$1"
    local sock; sock="$(_admin_socket_for "$name")"
    if [[ -z "$sock" || ! -S "$sock" ]]; then
        pw "Admin socket not available (is the tunnel running, and admin.enabled in its config?)"; pause; return
    fi
    header
    p "  ${BOLD}Benchmark — ${name}${RST}"
    p "  ${DIM}Runs over the live tunnel; only meaningful in client mode.${RST}"
    br
    printf "  Duration in seconds [5]: "; read -r dur; dur="${dur:-5}"
    printf "  Parallel streams (throughput only) [4]: "; read -r par; par="${par:-4}"
    br
    pi "Latency..."
    "$BINARY" admin --socket "$sock" -H bench latency "${dur}s" 2>&1 || pe "bench latency failed"
    br
    pi "Throughput..."
    "$BINARY" admin --socket "$sock" -H bench throughput "${dur}s" "$par" 2>&1 || pe "bench throughput failed"
    pause
}

# ── New tunnel wizard ─────────────────────────────────────────────────────────
tunnel_new_wizard() {
    local mode="${1:-server}"   # server=Iran  client=Foreign

    header
    require_python

    if [[ "$mode" == "server" ]]; then
        p  "  ${BOLD}${W}New Tunnel — Iran (server)${RST}"
        local label="Iran"
        local default_name="iran-main"
        local default_tun_local="10.10.20.1"
        local default_tun_remote="10.10.20.2"
        local default_spoof_src="$DEFAULT_IRAN_SPOOF_SRC"
        local default_spoof_dst="$DEFAULT_IRAN_SPOOF_DST"
    else
        p  "  ${BOLD}${W}New Tunnel — Foreign (client)${RST}"
        local label="Foreign"
        local default_name="foreign-main"
        local default_tun_local="10.10.20.2"
        local default_tun_remote="10.10.20.1"
        local default_spoof_src="$DEFAULT_FOREIGN_SPOOF_SRC"
        local default_spoof_dst="$DEFAULT_FOREIGN_SPOOF_DST"
    fi

    p  "  ${DIM}Press Ctrl+C to cancel.${RST}"
    br

    # ── Name ─────────────────────────────────────────────────────────────────
    sep
    p  "  ${BOLD}Tunnel name${RST}"
    local name="" cfg=""
    while true; do
        printf "  [${default_name}]: "; read -r name
        name="${name:-$default_name}"
        if [[ ! "$name" =~ ^[A-Za-z0-9_-]+$ ]]; then
            pw "Only letters, numbers, '-' and '_' allowed (this name becomes a systemd unit and file name)"
            continue
        fi
        cfg="$(cfg_path "$name")"
        if [[ -f "$cfg" ]]; then
            pw "Config already exists: $cfg"
            printf "  Use a different name? [Y/n]: "; read -r ans
            [[ "${ans,,}" == "n" ]] && { pause; return; }
            name=""
        else
            break
        fi
    done

    # ── Performance tier ────────────────────────────────────────────────────
    br; sep
    p  "  ${BOLD}Performance tier${RST}"
    p  "  ${DIM}Sets obfuscation / QUIC pool size / congestion control / TUN queues.${RST}"
    p  "  ${DIM}Both sides of a tunnel must use the same tier.${RST}"
    br
    p  "  ${BOLD}1)${RST} Light   ${DIM}— 1-20 users, weak VPS${RST}"
    p  "  ${BOLD}2)${RST} Medium  ${DIM}— 20-100 users${RST}"
    p  "  ${BOLD}3)${RST} High    ${DIM}— 100-500 users${RST}"
    p  "  ${BOLD}4)${RST} Ultra   ${DIM}— thousands of users, dedicated box${RST}"
    br
    local sug_tier sug_cores sug_mem
    sug_tier="$(suggested_tier)"
    sug_cores="$(nproc 2>/dev/null || echo '?')"
    sug_mem="$(awk '/^MemTotal:/ {printf "%.1f", $2/1024/1024}' /proc/meminfo 2>/dev/null)"
    p  "  ${DIM}Detected: ${sug_cores} CPU core(s), ~${sug_mem:-?} GB RAM → suggested: ${sug_tier}${RST}"
    printf "  Tier [${sug_tier}]: "; read -r tier_choice
    local tier
    case "${tier_choice:-$sug_tier}" in
        1) tier="light" ;;
        3) tier="high" ;;
        4) tier="ultra" ;;
        *) tier="medium" ;;
    esac
    local tier_template="$TIERS_DIR/${tier}-${mode}.json"
    [[ -f "$tier_template" ]] || die "Tier template not found: $tier_template (expected next to the manager script under tiers/)"

    # ── IPs ──────────────────────────────────────────────────────────────────
    br; sep
    p  "  ${BOLD}IP Addresses${RST}"

    local detected_ip; detected_ip="$(local_ip)"
    local listen=""
    while [[ -z "$listen" ]]; do
        if [[ -n "$detected_ip" ]]; then
            printf "  Listen IP — this machine ${G}(detected: %s)${RST}\n  [${detected_ip}]: " "$detected_ip"
            read -r listen
            listen="${listen:-$detected_ip}"
        else
            printf "  Listen IP — this machine: "; read -r listen
        fi
        [[ -z "$listen" ]] && pw "Required — cannot be empty"
    done

    local dst=""
    while [[ -z "$dst" ]]; do
        printf "  Peer IP — remote machine: "; read -r dst
        [[ -z "$dst" ]] && pw "Required — cannot be empty"
    done

    # ── Spoof ────────────────────────────────────────────────────────────────
    br; sep
    p  "  ${BOLD}Spoof IPs${RST}"
    printf "  Spoof source IP — sent as UDP src   [${default_spoof_src}]: "; read -r spoof_src
    spoof_src="${spoof_src:-$default_spoof_src}"
    printf "  Spoof dest IP   — expected incoming [${default_spoof_dst}]: "; read -r spoof_dst
    spoof_dst="${spoof_dst:-$default_spoof_dst}"

    # ── Tunnel settings ───────────────────────────────────────────────────────
    br; sep
    p  "  ${BOLD}Tunnel settings${RST}"
    local port=""
    while true; do
        printf "  UDP port      [6262]: ";  read -r port;   port="${port:-6262}"
        [[ "$port" =~ ^[0-9]+$ ]] && (( port >= 1 && port <= 65535 )) && break
        pw "Port must be a number between 1 and 65535"
    done

    local suggested_tname; suggested_tname="$(next_free_tun_name)"
    local tname="" owner
    while true; do
        printf "  TUN interface [${suggested_tname}]: "; read -r tname
        tname="${tname:-$suggested_tname}"
        if [[ ! "$tname" =~ ^[A-Za-z0-9_-]{1,15}$ ]]; then
            pw "Interface name must be 1-15 chars, letters/numbers/-/_ only (Linux ifname limit)"
            continue
        fi
        owner="$(tun_name_owner "$tname" "$name")" || true
        if [[ -n "$owner" ]]; then
            pw "Interface '$tname' is already used by tunnel '$owner' on this box — pick another"
            continue
        fi
        break
    done
    printf "  TUN local IP  [${default_tun_local}]: ";  read -r tlocal
    tlocal="${tlocal:-$default_tun_local}"
    printf "  TUN peer IP   [${default_tun_remote}]: "; read -r tremote
    tremote="${tremote:-$default_tun_remote}"

    # ── Keys ─────────────────────────────────────────────────────────────────
    br; sep
    p  "  ${BOLD}Encryption keys${RST}"
    require_root
    mkdir -p "$CONFIG_DIR"
    local keyfile="${CONFIG_DIR}/${name}.key"
    local keygen_out; keygen_out="$("$BINARY" keygen --out-private "$keyfile" 2>&1)" \
        || die "keygen failed:
$keygen_out"
    local own_pub; own_pub="$(printf '%s\n' "$keygen_out" | grep "Public Key" | awk '{print $4}')"
    local own_priv; own_priv="$(cat "$keyfile")"
    pok "Generated key pair, private key saved: $keyfile"
    p  "  ${BOLD}Your public key (share with the peer):${RST}"
    p  "  ${W}${own_pub}${RST}"
    br
    local peer_pub=""
    while true; do
        printf "  Peer's public key: "; read -r peer_pub
        if [[ -z "$peer_pub" ]]; then
            pw "Required — cannot be empty"; continue
        fi
        # X25519 key, base64: 32 bytes -> 44 chars incl. the '=' pad.
        # Also guards the config-writing step below, which embeds this
        # verbatim into a JSON string inside an unquoted heredoc — a
        # pasted value with a stray quote or newline would otherwise
        # break that Python script instead of failing with a clear
        # message here.
        if [[ ! "$peer_pub" =~ ^[A-Za-z0-9+/]{43}=$ ]]; then
            pw "Doesn't look like a valid key (expected 44 base64 chars, e.g. from 'quiccochet keygen'). Check for a copy-paste truncation."
            continue
        fi
        break
    done

    # ── Server-only: this peer's TUN address ────────────────────────────────
    local peer_tun_addr="$tremote"
    local peer_name=""
    if [[ "$mode" == "server" ]]; then
        printf "  Peer name [vpn1]: "; read -r peer_name; peer_name="${peer_name:-vpn1}"
    fi

    # ── Port forwards (Iran/server only) ────────────────────────────────────
    local fwd_rules_json="[]"
    if [[ "$mode" == "server" ]]; then
        br; sep
        p  "  ${BOLD}Port Forwards${RST} ${DIM}(reverse: server listens, client dials target)${RST}"
        p  "  ${DIM}With TUN mode you can usually reach the client directly at its TUN${RST}"
        p  "  ${DIM}address (${tremote}) instead — use this only for exposing a fixed${RST}"
        p  "  ${DIM}port on this server's public IP. Leave empty to skip.${RST}"
        br

        local rules=() idx=1
        while true; do
            printf "  Forward #${idx} — listen port on this server (Enter to finish): "
            read -r lport
            [[ -z "$lport" ]] && break
            if [[ ! "$lport" =~ ^[0-9]+$ ]] || (( lport < 1 || lport > 65535 )); then
                pw "Port must be a number between 1 and 65535"; continue
            fi
            printf "  Target on the peer (host:port) [127.0.0.1:${lport}]: "
            read -r target
            target="${target:-127.0.0.1:${lport}}"
            rules+=("{\"listen\":\"${lport}\",\"target\":\"${target}\",\"peer\":\"${peer_name}\"}")
            ((idx++))
        done
        if [[ ${#rules[@]} -gt 0 ]]; then
            fwd_rules_json="[$(IFS=,; echo "${rules[*]}")]"
        fi
    fi

    # ── Write config: start from the tier template, override the specifics ──
    require_root
    python3 - "$tier_template" "$cfg" <<PYEOF
import json, sys
template_path, out_path = sys.argv[1], sys.argv[2]
with open(template_path) as f:
    cfg = json.load(f)

for k in list(cfg.keys()):
    if k.startswith("_"):
        del cfg[k]
cfg["_tier_name"] = "${tier}"

cfg["mode"] = "${mode}"
cfg["listen_port"] = ${port}
cfg["spoof"]["source_ips"] = ["${spoof_src}"]
cfg["crypto"]["private_key"] = "${own_priv}"
cfg["tun"]["name"] = "${tname}"
cfg["tun"]["local"] = "${tlocal}/24"
cfg["admin"] = {"enabled": True, "socket": "/run/${SVC_PREFIX}-${name}.sock"}

if "${mode}" == "server":
    cfg["peers"] = [{
        "name": "${peer_name}",
        "peer_public_key": "${peer_pub}",
        "client_real_ip": "${dst}",
        "peer_spoof_ips": ["${spoof_dst}"],
        "tun_addr": "${peer_tun_addr}",
    }]
    fwd = json.loads('${fwd_rules_json}')
    if fwd:
        cfg["reverse_forwards"] = fwd
    cfg.pop("server", None)
else:
    cfg["server"] = {"address": "${dst}", "port": ${port}}
    cfg["spoof"]["peer_spoof_ips"] = ["${spoof_dst}"]
    cfg["crypto"]["peer_public_key"] = "${peer_pub}"
    cfg.pop("peers", None)

with open(out_path, "w") as f:
    json.dump(cfg, f, indent=2)
PYEOF

    chmod 600 "$cfg"

    br
    pok "Config written: $cfg"
    br

    _install_svc "$name"
    br
    pi "Starting service..."
    if systemctl start "$(svc_name "$name")"; then
        pok "Service started: $(svc_name "$name")"
    else
        pe "Failed to start. Check logs:"
        pe "  journalctl -u $(svc_name "$name") -n 40"
    fi

    pause
}

# ── Install systemd unit ──────────────────────────────────────────────────────
_install_svc() {
    local name="$1"
    require_binary
    local cfg; cfg="$(cfg_path "$name")"
    [[ -f "$cfg" ]] || die "Config not found: $cfg"

    local svc; svc="$(svc_name "$name")"
    local unit="/etc/systemd/system/${svc}.service"
    local tun; tun="$(json_get "$cfg" "tun.name" "")"
    local mode; mode="$(json_get "$cfg" "mode" "server")"
    local port
    if [[ "$mode" == "server" ]]; then
        port="$(json_get "$cfg" "listen_port" "6262")"
    else
        port="$(json_get "$cfg" "server.port" "6262")"
    fi

    cat > "$unit" << EOF
[Unit]
Description=QUICochet Tunnel — ${name}
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStartPre=-/bin/bash -c 'fuser -k ${port}/udp 2>/dev/null; /sbin/ip link delete ${tun:-qc0} 2>/dev/null; sleep 0.5'
ExecStart=${BINARY} -c ${cfg}
Restart=always
RestartSec=5s
LimitNOFILE=1048576
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable "$svc" &>/dev/null
    pok "Service installed & enabled: $svc"
}

_install_restart_timer() {
    local name="$1"
    local minutes="$2"
    local svc; svc="$(svc_name "$name")"
    local restart_unit; restart_unit="$(restart_unit_name "$name")"
    local timer; timer="$(timer_name "$name")"

    systemctl list-unit-files "${svc}.service" --no-legend 2>/dev/null | grep -q "${svc}.service" \
        || die "Service not installed: ${svc}.service"

    cat > "/etc/systemd/system/${restart_unit}" << EOF
[Unit]
Description=Restart ${svc}
Wants=${svc}.service
After=${svc}.service

[Service]
Type=oneshot
ExecStart=/bin/systemctl restart ${svc}.service
EOF

    cat > "/etc/systemd/system/${timer}" << EOF
[Unit]
Description=Periodic restart timer for ${svc}

[Timer]
OnBootSec=${minutes}min
OnUnitActiveSec=${minutes}min
Unit=${restart_unit}
Persistent=true

[Install]
WantedBy=timers.target
EOF

    systemctl daemon-reload
    systemctl enable --now "$timer" &>/dev/null
    pok "Auto restart timer enabled: $timer (every ${minutes} minute(s))"
}

_disable_restart_timer() {
    local name="$1"
    local restart_unit; restart_unit="$(restart_unit_name "$name")"
    local timer; timer="$(timer_name "$name")"
    systemctl disable --now "$timer" 2>/dev/null || true
    rm -f "/etc/systemd/system/${timer}" "/etc/systemd/system/${restart_unit}"
    systemctl daemon-reload
    pok "Auto restart timer disabled: $timer"
}

_remove_svc() {
    local name="$1"
    _disable_restart_timer "$name" 2>/dev/null || true
    local svc; svc="$(svc_name "$name")"
    systemctl stop    "$svc" 2>/dev/null || true
    systemctl disable "$svc" 2>/dev/null || true
    rm -f "/etc/systemd/system/${svc}.service"
    systemctl daemon-reload
    pok "Service removed: $svc"
}

# Full teardown for one tunnel: service, config, private key, TUN
# interface. Shared by the TUI menu and the `remove` CLI command so
# there's exactly one place that has to remember all four — the config
# and TUN name must be read out *before* the config file is deleted.
_remove_tunnel() {
    local name="$1"
    local cfg; cfg="$(cfg_path "$name")"
    local tun_iface=""
    [[ -f "$cfg" ]] && tun_iface="$(json_get "$cfg" "tun.name" "")"
    _remove_svc "$name"
    rm -f "$cfg" "${CONFIG_DIR}/${name}.key"
    # The interface is usually already gone (tunnel was stopped, or
    # never started) — that's success, not a failure to report. Only a
    # non-"no such device" error should be worth surfacing, and even
    # then this is best-effort cleanup, not something to fail loudly.
    if [[ -n "$tun_iface" ]]; then
        ip link delete "$tun_iface" 2>/dev/null || true
    fi
    return 0
}

# ── CLI mode ──────────────────────────────────────────────────────────────────
cli_help() {
    p  "Usage: $(basename "$0") [command] [tunnel-name] [args...]"
    br
    p  "Commands:"
    p  "  (none)                  Interactive TUI"
    p  "  start   <name>          Start a tunnel service"
    p  "  stop    <name>          Stop a tunnel service"
    p  "  restart <name>          Restart a tunnel service"
    p  "  status  [name]          Show status (all or specific)"
    p  "  log     <name>          Follow live logs"
    p  "  list                    List all configured tunnels"
    p  "  install <name>          Install systemd service for existing config"
    p  "  timer   <name> <min>    Enable/edit periodic restart timer in minutes (e.g. 5, 30, 60)"
    p  "  timer-off <name>        Disable periodic restart timer"
    p  "  stats   <name>          Dump live admin-socket stats"
    p  "  bench   <name> [dur]    Run latency + throughput benchmark over the live tunnel"
    p  "  remove  <name>          Remove service and config"
    p  "  keygen                  Generate a new key pair"
    br
    p  "To update the binary:"
    p  "  1. Replace $BINARY with the new build"
    p  "  2. Run: $(basename "$0") restart <name>"
}

cli_cmd() {
    local cmd="${1:-}"; shift 2>/dev/null || true
    local name="${1:-}"
    case "$cmd" in
        start)   require_root; systemctl start   "$(svc_name "$name")" ;;
        stop)    require_root; systemctl stop    "$(svc_name "$name")" ;;
        restart) require_root; systemctl restart "$(svc_name "$name")" ;;
        status)
            if [[ -n "$name" ]]; then
                systemctl status "$(svc_name "$name")" --no-pager
            else
                mapfile -t tl < <(list_tunnels)
                for t in "${tl[@]}"; do
                    printf "  %-24s %s\n" "$t" "$(svc_state "$t")"
                done
            fi ;;
        log|logs)  journalctl -u "$(svc_name "$name")" -f --no-pager ;;
        list)      list_tunnels ;;
        install)   require_root; _install_svc "$name" ;;
        timer)     require_root
                   local minutes_raw="${2:-}" minutes
                   minutes="$(normalize_restart_minutes "$minutes_raw")" || die "Invalid value. Use minutes only, like 5 or 30."
                   [[ "$minutes" == "off" ]] && _disable_restart_timer "$name" || _install_restart_timer "$name" "$minutes" ;;
        timer-off) require_root; _disable_restart_timer "$name" ;;
        stats)     require_python; _admin_stats "$name" ;;
        bench)
            require_python
            local sock; sock="$(_admin_socket_for "$name")"
            [[ -n "$sock" && -S "$sock" ]] || die "Admin socket not available (is the tunnel running?)"
            local dur="${2:-5}"
            "$BINARY" admin --socket "$sock" -H bench latency "${dur}s"
            "$BINARY" admin --socket "$sock" -H bench throughput "${dur}s" 4 ;;
        remove)    require_root; _remove_tunnel "$name" ;;
        keygen)    require_binary; "$BINARY" keygen ;;
        help|--help|-h) cli_help ;;
        *) pw "Unknown command: $cmd"; br; cli_help; exit 1 ;;
    esac
}

# ── Entry ─────────────────────────────────────────────────────────────────────
if [[ $# -eq 0 ]]; then
    require_python
    main_menu
else
    cli_cmd "$@"
fi
