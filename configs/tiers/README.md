# Tier presets

Four ready-made config pairs (server + client), matching the refactor
spec's tier table. Each is a normal JSON config — copy the pair for
your role, fill in the `SERVER_IP` / `*_SPOOF_IP` / `CLIENT_REAL_IP`
placeholders and the `crypto.private_key` / `peer_public_key` fields
(`quiccochet keygen`), then run:

```
quiccochet -c <tier>-server.json    # on the Iran-side / server box
quiccochet -c <tier>-client.json    # on the foreign-side / client box
```

| Tier | Scenario | Obfuscation | `quic.pool_size` | Congestion | `tun.queues` / `pin_cores` | Buffers |
|---|---|---|---|---|---|---|
| **light** | 1-20 users, weak VPS | none | 2 | cubic | 1 / off | 4 MB |
| **medium** | 20-100 users | none | 4 | auto (bbrv1→cubic) | 2 / off | 8 MB |
| **high** | 100-500 users | none (switch to `standard` under active DPI) | 10 | bbrv1 | 4 / on | 24 MB |
| **ultra** | thousands of users, dedicated box | none | 16 | bbrv1 | 8 / on (set to your core count) | 32 MB |

Notes:

- **`tun.queues`** is this refactor's answer to "worker/thread count"
  in the original spec table — see docs/PROFILING-BASELINE.md and the
  batched-I/O commit for why: `/dev/net/tun` has no `recvmmsg`
  equivalent, so parallel `IFF_MULTI_QUEUE` queues (one goroutine
  each) are what actually scales packet processing across cores.
  `high`/`ultra` assume an 8-core box; adjust to your real core count.
- **`obfuscation.mode`** defaults to `none` on every tier, including
  `high` and `ultra` — the spec's own baseline analysis
  (docs/PROFILING-BASELINE.md) found padding/chaffing is a real but
  secondary CPU cost, not the dominant one, so it's opt-in rather than
  a tax every deployment pays. Enable `standard` only where the path
  is under active DPI.
- **`ultra`**'s `performance.pacing_rate_mbps` ships at `0`
  (disabled) — set it once you know your box's real uplink capacity
  (e.g. `900` for a 1 Gbps NIC with headroom); see the field's doc
  comment in `internal/config/config.go` for the `tc qdisc` prerequisite.
- All four were validated end-to-end on this branch: `quiccochet -c
  <tier>-*.json` starts cleanly (config passes `Validate()`, TUN
  device opens with the configured queue count), and the `medium`
  pair was run through a full client↔server session — QUIC pool at
  the configured `pool_size`, `admin bench throughput`/`latency`, and
  a real SOCKS5 relay fetch all worked correctly.
