#!/usr/bin/env bash
# =============================================================================
# install.sh — one-line installer for the QUICochet tunnel manager
#
#   bash <(curl -fsSL https://raw.githubusercontent.com/originnova555-hue/esp-tun/claude/quiccochet-tunnel-refactor-owaip5/install.sh)
#
# Installs the quiccochet daemon together with the spoof-tunnel.sh
# manager and the four performance-tier templates under one directory,
# then — on an interactive terminal — drops straight into the manager's
# menu. Re-run any time with `spoof-tunnel`.
#
# Install order: prebuilt release for this CPU (checksum-verified) →
# build from source if no matching release asset is available.
# =============================================================================
set -euo pipefail

REPO_SLUG="${REPO_SLUG:-originnova555-hue/esp-tun}"
REPO_URL="${REPO_URL:-https://github.com/${REPO_SLUG}.git}"
REPO_BRANCH="${REPO_BRANCH:-claude/quiccochet-tunnel-refactor-owaip5}"
RELEASE_BASE="${RELEASE_BASE:-https://github.com/${REPO_SLUG}/releases/latest/download}"
INSTALL_DIR="${INSTALL_DIR:-/opt/quiccochet}"
GO_MIN_VERSION="1.25"
GO_INSTALL_VERSION="1.25.8"
# Set FORCE_SOURCE=1 to skip the release download and always build.
FORCE_SOURCE="${FORCE_SOURCE:-0}"

C=$'\e[0;36m' Y=$'\e[1;33m' R=$'\e[0;31m' G=$'\e[0;32m' RST=$'\e[0m'
info() { printf "${C}[*]${RST} %s\n" "$*"; }
warn() { printf "${Y}[!]${RST} %s\n" "$*" >&2; }
err()  { printf "${R}[x]${RST} %s\n" "$*" >&2; }
ok()   { printf "${G}[*]${RST} %s\n" "$*"; }
die()  { err "$*"; exit 1; }

[[ $EUID -eq 0 ]] || die "Run as root (sudo bash install.sh), or: curl -fsSL <url> | sudo bash"
[[ "$(uname -s)" == "Linux" ]] || die "QUICochet requires Linux (raw sockets, TUN, IP_TRANSPARENT)."

TMPDIR_SELF="$(mktemp -d /tmp/quiccochet-install.XXXXXX)"
CLONE_DIR=""
cleanup() {
    rm -rf "$TMPDIR_SELF"
    # Only ever remove a clone this script made itself — never a
    # checkout the user was running us from.
    [[ -n "$CLONE_DIR" && "$CLONE_DIR" == /tmp/quiccochet-src.* ]] && rm -rf "$CLONE_DIR"
    return 0
}
trap cleanup EXIT

# ── CPU architecture → release asset suffix ────────────────────────────────
case "$(uname -m)" in
    x86_64|amd64)   ARCH_SUFFIX="linux-amd64" ;;
    aarch64|arm64)  ARCH_SUFFIX="linux-arm64" ;;
    armv7l|armv7|armhf) ARCH_SUFFIX="linux-armv7" ;;
    *)              ARCH_SUFFIX="" ;;
esac

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || true)"
have_local_checkout() {
    [[ -n "$SELF_DIR" && -f "$SELF_DIR/go.mod" && -d "$SELF_DIR/cmd/quiccochet" ]]
}

# ── Path A: prebuilt release ───────────────────────────────────────────────
# Downloads the toolkit tarball for this CPU and verifies it against the
# release's published checksums.txt before unpacking anything.
install_from_release() {
    [[ "$FORCE_SOURCE" == "1" ]] && { info "FORCE_SOURCE=1 — skipping the release download."; return 1; }
    [[ -n "$ARCH_SUFFIX" ]] || { warn "No prebuilt release for CPU $(uname -m) — will build from source."; return 1; }
    command -v curl >/dev/null 2>&1 || { warn "curl not found — will build from source."; return 1; }

    local tarball="quiccochet-${ARCH_SUFFIX}.tar.gz"
    local tar_path="${TMPDIR_SELF}/${tarball}"
    local sums_path="${TMPDIR_SELF}/checksums.txt"

    info "Fetching prebuilt release for ${ARCH_SUFFIX}..."
    # -fsL, deliberately without -S: this probe is allowed to fail (no
    # release published yet for this branch/arch) and we print our own
    # explanation below. --show-error here would spray a raw
    # "curl: (22) ... 404" at the user right before that message.
    if ! curl -fsL "${RELEASE_BASE}/${tarball}" -o "$tar_path"; then
        warn "No published release asset ${tarball} (yet) — falling back to a source build."
        return 1
    fi
    if ! curl -fsSL "${RELEASE_BASE}/checksums.txt" -o "$sums_path"; then
        die "Downloaded ${tarball} but its checksums.txt is missing — refusing to install an unverified binary."
    fi

    local expected actual
    expected="$(awk -v f="$tarball" '$2 == f || $2 == "*"f {print $1}' "$sums_path" | head -1)"
    [[ -n "$expected" ]] || die "checksums.txt has no entry for ${tarball} — refusing to install an unverified binary."
    if command -v sha256sum >/dev/null 2>&1; then
        actual="$(sha256sum "$tar_path" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        actual="$(shasum -a 256 "$tar_path" | awk '{print $1}')"
    else
        die "Neither sha256sum nor shasum available — cannot verify the download."
    fi
    if [[ "$actual" != "$expected" ]]; then
        rm -f "$tar_path"
        die "Checksum mismatch for ${tarball} (expected ${expected}, got ${actual}). Refusing to install."
    fi
    ok "Checksum verified."

    # The tarball unpacks to a single top-level quiccochet/ directory
    # holding the binary, the manager script, and tiers/.
    tar -C "$TMPDIR_SELF" -xzf "$tar_path" || die "Failed to unpack ${tarball}."
    local unpacked="${TMPDIR_SELF}/quiccochet"
    [[ -x "${unpacked}/quiccochet" && -f "${unpacked}/spoof-tunnel.sh" && -d "${unpacked}/tiers" ]] \
        || die "Unexpected tarball layout in ${tarball}."

    mkdir -p "$INSTALL_DIR"
    install -m 0755 "${unpacked}/quiccochet"      "${INSTALL_DIR}/quiccochet"
    install -m 0755 "${unpacked}/spoof-tunnel.sh" "${INSTALL_DIR}/spoof-tunnel.sh"
    rm -rf "${INSTALL_DIR}/tiers"
    cp -r "${unpacked}/tiers" "${INSTALL_DIR}/tiers"
    ok "Installed prebuilt ${ARCH_SUFFIX} build."
    return 0
}

# ── Path B: build from source ──────────────────────────────────────────────
ensure_go() {
    local v
    if command -v go >/dev/null 2>&1; then
        v="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
        if [[ -n "$v" ]] && printf '%s\n%s\n' "$GO_MIN_VERSION" "$v" | sort -C -V; then
            info "Found $(go version)"
            return 0
        fi
    fi
    warn "No Go toolchain >= ${GO_MIN_VERSION} found — installing Go ${GO_INSTALL_VERSION} to /usr/local/go"
    local goarch
    case "$(uname -m)" in
        x86_64|amd64)  goarch=amd64 ;;
        aarch64|arm64) goarch=arm64 ;;
        armv7l|armv7|armhf) goarch=armv6l ;;
        *) die "Unsupported CPU architecture for an automatic Go install: $(uname -m). Install Go ${GO_MIN_VERSION}+ manually and re-run." ;;
    esac
    command -v curl >/dev/null 2>&1 || die "curl is required to download the Go toolchain."
    local tarball="go${GO_INSTALL_VERSION}.linux-${goarch}.tar.gz"
    curl -fsSL "https://go.dev/dl/${tarball}" -o "${TMPDIR_SELF}/${tarball}" \
        || die "Failed to download the Go toolchain from go.dev. Install Go ${GO_MIN_VERSION}+ manually and re-run."
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "${TMPDIR_SELF}/${tarball}"
    export PATH="/usr/local/go/bin:$PATH"
    command -v go >/dev/null 2>&1 || die "Go install did not produce a working toolchain."
    ok "Installed $(go version)"
}

install_from_source() {
    local src
    if have_local_checkout; then
        src="$SELF_DIR"
        info "Using local checkout: $src"
    else
        command -v git >/dev/null 2>&1 || die "git is required to fetch the source (or run this script from inside an existing checkout)."
        CLONE_DIR="$(mktemp -d /tmp/quiccochet-src.XXXXXX)"
        src="$CLONE_DIR"
        info "Cloning ${REPO_URL} (branch ${REPO_BRANCH})..."
        git clone --quiet --depth 1 --branch "$REPO_BRANCH" "$REPO_URL" "$src" \
            || die "Clone failed. Check REPO_URL/REPO_BRANCH, or run this script from inside an existing checkout."
    fi

    ensure_go
    info "Building quiccochet (this compiles the vendored quic-go fork — a minute or two)..."
    mkdir -p "$INSTALL_DIR"
    (
        cd "$src"
        local version commit buildtime
        version="$(git describe --tags --always 2>/dev/null || echo dev)"
        commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
        buildtime="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        CGO_ENABLED=0 go build \
            -ldflags "-s -w -X main.Version=${version} -X main.Commit=${commit} -X main.BuildTime=${buildtime}" \
            -o "${INSTALL_DIR}/quiccochet" ./cmd/quiccochet/
    ) || die "Build failed."
    chmod 755 "${INSTALL_DIR}/quiccochet"

    install -m 0755 "$src/scripts/spoof-tunnel.sh" "${INSTALL_DIR}/spoof-tunnel.sh"
    rm -rf "${INSTALL_DIR}/tiers"
    cp -r "$src/configs/tiers" "${INSTALL_DIR}/tiers"
    ok "Built and installed from source."
}

install_from_release || install_from_source

# ── Convenience symlink so `spoof-tunnel` works from anywhere ──────────────
ln -sf "$INSTALL_DIR/spoof-tunnel.sh" /usr/local/bin/spoof-tunnel

echo
ok "Install complete — $("${INSTALL_DIR}/quiccochet" --version 2>/dev/null || echo 'quiccochet')"
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
