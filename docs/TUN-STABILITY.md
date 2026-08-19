# TUN persistence & spoof-IP health-check under TUN/L3 mode

Verification that the spec's connection-stability requirements still
hold after the SOCKS5 relay → TUN/L3 refactor: the spoof-IP health-
check/quarantine mechanism (`internal/transport/srcpool.go`) and the
resilient QUIC pool reconnect loop (`Client.maintainPool`) were not
modified by the TUN work, and the TUN device's lifecycle is
independent of both, as designed.

## Code-level check

`tunDevs` (client and server) is touched in exactly four places:
declared on the struct, assigned once in `Start` (`tun.OpenQueues`),
read from the TUN read/write goroutines, and closed once in `Stop`.
`maintainPool` — the loop that detects dead QUIC connections and
calls `SrcPool.MarkConnDeadV4/V6` to quarantine a misbehaving spoof
IP — was not changed by this refactor and has no reference to
`tunDevs` at all. There is no code path between a QUIC reconnect (or
a spoof-IP rotation, which forces one) and the TUN device's fd.

## Runtime verification

Ran a live client/server pair with `tun.enabled=true` on both sides,
`pool_size=2`, and a short idle timeout (3s) to force reconnects
quickly:

1. **Baseline**: `admin stats` showed `pool 2/2`, TUN interface up on
   both ends with the configured addresses.
2. **Killed the server process** (`kill -9`) while the client kept
   running. Client logs show the datagram receivers timing out
   ("no recent network activity"), then `pool tick: reconnecting dead
   slots` firing — the existing resilient-pool logic detecting the
   failure and starting its reconnect loop, unchanged.
3. **While the pool was fully dead** (`admin stats` → `pool 0/2`),
   confirmed via `ip addr show` that the client's TUN interface (name,
   address, MTU, UP flag) was completely untouched — it never
   depends on a live QUIC connection to stay configured.
4. **Restarted the server.** Client logs show `pool reconnect: dial
   returned ok=true` and `pool restored` within one tick — fully
   automatic, no process restart or manual intervention on either
   side. `admin stats` returned to `pool 2/2`.
5. **Sent a real UDP packet through the TUN device** post-recovery
   (client TUN address → server TUN address, echoed back): received
   correctly, confirming the tunnel is not just administratively "up"
   but actually passing traffic again through the same TUN device
   that never went down.

This confirms the spec's stability requirements
("the TUN interface must stay persistent... don't recreate it on
every reconnect", "keep the existing resilient reconnect pool") hold
for the new TUN/L3 datapath exactly as they did for the pre-refactor
SOCKS5 relay — the health-check/reconnect machinery operates one
layer below the TUN datapath and was never coupled to it in the first
place; the refactor only needed to preserve that separation, not add
new logic.
