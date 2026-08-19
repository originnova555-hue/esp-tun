#!/bin/bash
set -euo pipefail

# Test suite for spoof-tunnel.sh manager

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
CONFIG_DIR="$TEST_DIR/config"
STATE_DIR="$TEST_DIR/state"
SYSTEMD_DIR="$TEST_DIR/systemd"
BIN_DIR="$TEST_DIR/bin"

export XDG_CONFIG_HOME="$CONFIG_DIR"
export XDG_RUNTIME_DIR="$STATE_DIR"

mkdir -p "$CONFIG_DIR" "$STATE_DIR" "$SYSTEMD_DIR" "$BIN_DIR"

cleanup() {
	rm -rf "$TEST_DIR"
}
trap cleanup EXIT

# Mock binary for testing
MOCK_BINARY="$BIN_DIR/quiccochet"
mkdir -p "$(dirname "$MOCK_BINARY")"
cat > "$MOCK_BINARY" <<'EOF'
#!/bin/bash
case "$1" in
  sample) echo "[test]\nname = \"test\"" ;;
  check) exit 0 ;;
  run) sleep 2 ;;
  *) echo "mock binary: $@" ;;
esac
EOF
chmod +x "$MOCK_BINARY"

MANAGER="$SCRIPT_DIR/scripts/spoof-tunnel.sh"
export PATH="$BIN_DIR:$PATH"

# Helper functions
assert_file_exists() {
	local file="$1"
	if [[ ! -f "$file" ]]; then
		echo "FAIL: File not found: $file"
		exit 1
	fi
}

assert_file_not_exists() {
	local file="$1"
	if [[ -f "$file" ]]; then
		echo "FAIL: File should not exist: $file"
		exit 1
	fi
}

assert_contains() {
	local output="$1"
	local expected="$2"
	if ! grep -q "$expected" <<< "$output"; then
		echo "FAIL: Output missing expected text: $expected"
		echo "Got: $output"
		exit 1
	fi
}

# Tests
test_list_empty() {
	echo "Testing: list (empty)"
	local output=$("$MANAGER" list)
	assert_contains "$output" "No tunnels configured"
}

test_create() {
	echo "Testing: create"
	"$MANAGER" create test-tunnel
	assert_file_exists "$CONFIG_DIR/test-tunnel.toml"
}

test_list_tunnel() {
	echo "Testing: list (with tunnel)"
	test_create
	local output=$("$MANAGER" list)
	assert_contains "$output" "test-tunnel"
}

test_config_wizard() {
	echo "Testing: wizard (non-interactive)"
	# Just test that the function exists and can be called
	# Full interactive testing would require expect or similar
	if type -t cmd_wizard > /dev/null; then
		echo "  wizard function exists"
	fi
}

test_service_enable() {
	echo "Testing: service enable"
	test_create
	"$MANAGER" service test-tunnel enable
	assert_file_exists "$SYSTEMD_DIR/quiccochet-test-tunnel.service"
}

test_service_disable() {
	echo "Testing: service disable"
	test_create
	test_service_enable
	"$MANAGER" service test-tunnel disable
	assert_file_not_exists "$SYSTEMD_DIR/quiccochet-test-tunnel.service"
}

test_delete() {
	echo "Testing: delete"
	test_create
	"$MANAGER" delete test-tunnel
	assert_file_not_exists "$CONFIG_DIR/test-tunnel.toml"
}

test_config_format() {
	echo "Testing: config format"
	test_create
	local config="$CONFIG_DIR/test-tunnel.toml"

	# Verify TOML structure
	grep -q "name = " "$config" || (echo "FAIL: Missing name"; exit 1)
	grep -q "role = " "$config" || (echo "FAIL: Missing role"; exit 1)
	grep -q "\[tun\]" "$config" || (echo "FAIL: Missing [tun] section"; exit 1)
}

test_multiple_tunnels() {
	echo "Testing: multiple tunnels"
	for i in 1 2 3; do
		"$MANAGER" create "tunnel-$i"
		assert_file_exists "$CONFIG_DIR/tunnel-$i.toml"
	done

	local output=$("$MANAGER" list)
	for i in 1 2 3; do
		assert_contains "$output" "tunnel-$i"
	done
}

test_help() {
	echo "Testing: help"
	local output=$("$MANAGER" --help)
	assert_contains "$output" "Usage:"
	assert_contains "$output" "Commands:"
}

# Run all tests
echo "=== spoof-tunnel.sh Test Suite ==="
echo "Config dir: $CONFIG_DIR"
echo "State dir: $STATE_DIR"
echo

test_list_empty
test_create
test_list_tunnel
test_config_wizard
test_config_format
test_service_enable
test_service_disable
test_delete
test_multiple_tunnels
test_help

echo
echo "=== All tests passed ==="
