#!/usr/bin/env bash
# =============================================================================
# install.sh — one-line installer for the QUICochet tunnel manager
#
#   bash <(curl -fsSL https://raw.githubusercontent.com/originnova555-hue/esp-tun/claude/quiccochet-tunnel-refactor-owaip5/install.sh)
#
# Builds quiccochet from source (no prebuilt binaries are published for this
# fork), installs it alongside the spoof-tunnel.sh manager and the four
# performance-tier config templates under one directory, and — on an
# interactive terminal — drops straight into the manager's menu. Re-run any
# time with `spoof-tunnel` (a symlink this script installs) to reopen it.
# =============================================================================
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/originnova555-hue/esp-tun.git}"
REPO_BRANCH="${REPO_BRANCH:-claude/quiccochet-tunnel-refactor-owaip5}"
INSTALL_DIR="${INSTALL_DIR:-/opt/quiccochet}"
GO_MIN_VERSION="1.25"
GO_INSTALL_VERSION="1.25.8"

C=$'\e[0;36m' Y=$'\e[1;33m' R=$'\e[0;31m' G=$'\e[0;32m' RST=$'\e[0m'
info() { printf "${C}[*]${RST} %s\n" "$*"; }
warn() { printf "${Y}[!]${RST} %s\n" "$*" >&2; }
err()  { printf "${R}[x]${RST} %s\n" "$*" >&2; }
ok()   { printf "${G}[*]${RST} %s\n" "$*"; }
die()  { err "$*"; exit 1; }

[[ $EUID -eq 0 ]] || die "Run as root (sudo bash install.sh), or: curl -fsSL <url> | sudo bash"
[[ "$(uname -s)" == "Linux" ]] || die "QUICochet requires Linux (raw sockets, TUN, IP_TRANSPARENT)."

# ── 1. Source: use the local checkout if this script is run from inside one,
#    otherwise git clone the repo fresh into a scratch directory. ────────────
SRC_DIR=""
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || true)"
if [[ -n "$SELF_DIR" && -f "$SELF_DIR/go.mod" && -d "$SELF_DIR/cmd/quiccochet" ]]; then
    SRC_DIR="$SELF_DIR"
    info "Using local checkout: $SRC_DIR"
else
    command -v git >/dev/null 2>&1 || die "git is required to fetch the source (or run this script from inside an existing checkout)."
    SRC_DIR="$(mktemp -d /tmp/quiccochet-src.XXXXXX)"
    info "Cloning $REPO_URL (branch $REPO_BRANCH)..."
    git clone --quiet --depth 1 --branch "$REPO_BRANCH" "$REPO_URL" "$SRC_DIR" \
        || die "Clone failed. Check REPO_URL/REPO_BRANCH, or run this script from inside an existing checkout instead."
fi

# ── 2. Go toolchain: distro packages routinely lag the version this module
#    requires, so prefer the official upstream tarball over apt/dnf/pacman. ──
have_go_ge_min() {
    command -v go >/dev/null 2>&1 || return 1
    local v; v="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
    [[ -n "$v" ]] || return 1
    printf '%s\n%s\n' "$GO_MIN_VERSION" "$v" | sort -C -V
}

if have_go_ge_min; then
    info "Found $(go version)"
else
    warn "No Go toolchain >= ${GO_MIN_VERSION} found — installing Go ${GO_INSTALL_VERSION} to /usr/local/go"
    arch="$(uname -m)"
    case "$arch" in
        x86_64|amd64)  goarch=amd64 ;;
        aarch64|arm64) goarch=arm64 ;;
        *) die "Unsupported CPU architecture: $arch (need amd64 or arm64)" ;;
    esac
    tarball="go${GO_INSTALL_VERSION}.linux-${goarch}.tar.gz"
    tmp_tar="$(mktemp /tmp/${tarball}.XXXXXX)"
    command -v curl >/dev/null 2>&1 || die "curl is required to download the Go toolchain."
    curl -fsSL "https://go.dev/dl/${tarball}" -o "$tmp_tar" \
        || die "Failed to download the Go toolchain from go.dev. Install Go ${GO_MIN_VERSION}+ manually and re-run."
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tmp_tar"
    rm -f "$tmp_tar"
    export PATH="/usr/local/go/bin:$PATH"
    have_go_ge_min || die "Go install did not produce a working >= ${GO_MIN_VERSION} toolchain."
    ok "Installed $(go version)"
fi

# ── 3. Build ───────────────────────────────────────────────────────────────
info "Building quiccochet (this compiles the vendored quic-go fork — a minute or two)..."
mkdir -p "$INSTALL_DIR"
(
    cd "$SRC_DIR"
    version="$(git describe --tags --always 2>/dev/null || echo dev)"
    commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
    buildtime="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    go build -ldflags "-X main.Version=${version} -X main.Commit=${commit} -X main.BuildTime=${buildtime}" \
        -o "${INSTALL_DIR}/quiccochet" ./cmd/quiccochet/
) || die "Build failed."
chmod 755 "${INSTALL_DIR}/quiccochet"
ok "Built ${INSTALL_DIR}/quiccochet"

# ── 4. Manager script + tier templates, side by side with the binary
#    (spoof-tunnel.sh resolves both relative to its own location). ──────────
install -m 0755 "$SRC_DIR/scripts/spoof-tunnel.sh" "$INSTALL_DIR/spoof-tunnel.sh"
rm -rf "$INSTALL_DIR/tiers"
cp -r "$SRC_DIR/configs/tiers" "$INSTALL_DIR/tiers"
ok "Installed manager script and tier templates in $INSTALL_DIR"

# ── 5. Convenience symlink so `spoof-tunnel` works from anywhere ────────────
ln -sf "$INSTALL_DIR/spoof-tunnel.sh" /usr/local/bin/spoof-tunnel
ok "Linked /usr/local/bin/spoof-tunnel -> $INSTALL_DIR/spoof-tunnel.sh"

# ── 6. Clean up a clone we made ourselves (never touch a local checkout) ────
[[ "$SRC_DIR" == /tmp/quiccochet-src.* ]] && rm -rf "$SRC_DIR"

echo
ok "Install complete."
printf "    Binary:  %s\n" "${INSTALL_DIR}/quiccochet"
printf "    Manager: %s  (also: spoof-tunnel)\n" "${INSTALL_DIR}/spoof-tunnel.sh"
printf "    Tiers:   %s\n" "${INSTALL_DIR}/tiers"
echo

if [[ -t 0 && -t 1 ]]; then
    info "Opening the tunnel manager. Re-run any time with: sudo spoof-tunnel"
    echo
    exec "$INSTALL_DIR/spoof-tunnel.sh"
else
    info "Non-interactive shell — not opening the menu automatically."
    info "Start it with: sudo spoof-tunnel"
fi
