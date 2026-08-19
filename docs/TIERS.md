# Performance tiers

A tier is a **preset over the ordinary config surface** — `internal/config/tiers.go`
sets defaults, then your TOML is decoded on top. There is no tier-specific code
path anywhere in the engine, so a tier can never behave differently from the
equivalent hand-written config.

| | light | medium | high | ultra |
|---|---|---|---|---|
| target | 1-20 users, weak VPS | 20-100 users | 100-500 users | thousands |
| `quic.pool_size` | 2 | 4 | 8 | 16 |
| `quic.max_idle_timeout_sec` | 60 | 45 | 30 | 30 |
| `quic.keep_alive_period_sec` | 15 | 10 | 8 | 5 |
| reconnect backoff | 1s → 60s | 0.5s → 30s | 0.25s → 15s | 0.2s → 10s |
| `transport.rcv/snd_buffer_mb` | 4 | 8 | 24 | 32 |
| `datapath.workers` | 1 | 2 | 4 | 0 (one per core) |
| `datapath.pin_cores` | off | off | on | on |
| `datapath.batch_size` | 16 | 32 | 64 | 128 |
| `obfuscation.mode` | none | binning/256 | binning/256 | binning/512 |

## Why no tier turns padding on by default

Padding every packet up to the MTU is the single most expensive thing an
obfuscation layer can do. On a link carrying many small packets — DNS, HTTP
keep-alives, WebRTC, ACKs — it inflates real traffic several times over and
multiplies the AEAD cost by the same factor. That cost is paid on *every*
packet, forever, whether or not anything is inspecting the link.

So the ladder is:

1. **`none`** — the wire is already QUIC. To a classifier it looks like QUIC,
   because it *is* QUIC. Start here.
2. **`binning`** — frame lengths are rounded up to a multiple of `bin_size`,
   which flattens the length histogram for a few percent of overhead. This is
   the default from `medium` upward.
3. **`standard`** — binning plus chaff. Turn it on when you have actually
   observed active DPI on the path, not before.
4. **`paranoid`** — full-MTU padding, chaff and send jitter. Expect to give up
   a large fraction of your throughput. This is a last resort.

Raising a tier does **not** mean raising obfuscation. The two are independent
knobs and the higher tiers deliberately keep obfuscation low, because that is
where the throughput comes from.

## Higher jitter needs a higher idle timeout

`light` runs the longest idle timeout (60s) and the slowest reconnect backoff on
purpose: it targets weak boxes on lossy last-mile links, where an aggressive
timeout turns a two-second stall into a full session teardown. The higher tiers
assume a good path and can afford to notice a dead peer sooner.
