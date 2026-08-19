# Baseline CPU Profile — SOCKS5 UDP-ASSOCIATE Relay Model

Captured before the TUN/L3 refactor, on the current (pre-refactor) SOCKS5
per-flow relay architecture. This is the "before" side of the before/after
comparison called for in the refactor spec.

## Method

- Server + client, loopback, `pool_size=8`.
- Load generator: `test/loadgen` (Python prototype used for this baseline;
  see `many_flows.py` methodology below) opens **250 concurrent SOCKS5
  connections**, each doing a full SOCKS5 handshake + one small HTTP
  GET/response (~600 bytes), then closes — repeated continuously for 15s.
  This simulates the "hundreds of small concurrent flows (DNS, HTTP
  keep-alive, WebRTC)" scenario the spec calls out, not a single iperf3
  stream.
- CPU profile captured via the admin socket's pprof endpoint
  (`quiccochet admin pprof start`, then `go tool pprof -seconds=15
  http://127.0.0.1:6060/debug/pprof/profile`) concurrently with the load.
- Compared `obfuscation.mode=standard` vs `obfuscation.mode=none`.
- fd count sampled via `quiccochet admin stats` immediately after the load
  burst, and again 5s later.

Raw profiles: `docs/profiles/baseline-socks5-obfuscation-{standard,none}.pb.gz`
(open with `go tool pprof -top <file>` or `go tool pprof -http=:0 <file>`).

## Results

### Throughput / load handled

| obfuscation | flows completed (15s) | errors | flows/sec |
|---|---|---|---|
| standard | 9300 | 139 | 533.0 |
| none | 9102 | 200 | 528.2 |

Both handled roughly the same flow rate; the error rate under 250-way
concurrency (1.5-2%) is itself a symptom of the per-flow overhead below,
not link capacity — this is loopback.

### CPU profile top consumers (both modes, ~same shape)

| function | standard flat% | none flat% |
|---|---|---|
| `syscall.Syscall6` | 23.92% | 24.65% |
| `runtime.memclrNoHeapPointers` | 18.30% | 19.37% |
| `runtime.futex` | 10.43% | 11.27% |
| `runtime.stackpoolalloc` | 4.49% | 1.94% |
| `runtime.findRunnable` (cum) | 14.61% | — |
| AEAD encrypt (chacha20poly1305/AES-GCM) | **0.80%** | 0.53% |

Total CPU utilization over the 15s window: **41.25%** (standard) vs
**37.66%** (none) of one profiling budget — obfuscation adds roughly a
**9-10% relative** CPU tax under this load, consistent with the spec's
hypothesis but **not the dominant cost**.

### The actual bottleneck

Encryption itself is cheap — under 1% of CPU in both modes. The
overwhelming majority of CPU time goes to:

1. **`syscall.Syscall6` (~24%)** — one syscall per socket operation per
   flow: `net.ListenUDP`-per-assoc on the relay path, `dial`/`accept`/
   `read`/`write`/`close` per SOCKS5 TCP connection, per-stream QUIC
   framing. Every one of the 250 concurrent small flows does several of
   these independently.
2. **`runtime.memclrNoHeapPointers` (~19%)** — zeroing freshly allocated
   memory: each flow allocates its own goroutine stack, buffers
   (`proxyCopyPool`/`datagramPool` help but don't eliminate this), and
   per-stream/per-route bookkeeping structs.
3. **`runtime.futex` (~11%) + `findRunnable` (~15% cumulative)** — Go
   scheduler contention from running hundreds of concurrently-active
   goroutines (one per `handleStream`/`handleUDP`/receive-loop), each
   blocking and waking independently.
4. **`runtime.stackpoolalloc`** — new goroutine stack allocation, direct
   consequence of the per-flow goroutine model (`go s.handleStream(...)`
   for every accepted stream, `go c.receiveDatagrams(sess)`,
   `s.routeJanitor`, etc.).

This confirms the spec's hypothesis #2 and #3 in section 1: **per-flow
state management (fd, goroutine, route-table entry per SOCKS5
association/stream) is the dominant cost, not the AEAD cipher and not
`obfuscation.mode=standard`'s padding.** At 90% CPU under real multi-user
load (the field-reported symptom), this predicts a system with thousands
of short-lived goroutines and syscalls fighting the Go scheduler, not one
bottlenecked on crypto.

### fd behaviour under burst load

| moment | open fds |
|---|---|
| immediately after 250-way burst | 4427 (peak, transient) |
| 5s after load stops | 76 |

fds are reclaimed once the SOCKS5 relay tears down each flow's route/
socket, but the **peak** during a concurrent burst is disproportionate to
the steady-state session count (8 QUIC connections in the pool) — each of
the ~250 concurrent small flows briefly owns its own fd(s) via the
per-assoc `net.ListenUDP` / per-stream TCP dial in `handleStream`. This
is exactly the "hundreds of concurrent small flows ... per-connection
state management overhead" failure mode described in the spec.

## Conclusion — what the TUN/L3 refactor must fix

The profile does **not** point at crypto or the QUIC layer as the
bottleneck. It points at the **SOCKS5 UDP-ASSOCIATE / per-stream relay
model itself**: every inbound flow currently gets its own fd, its own
goroutine(s), and its own route-table entry, and the syscall +
scheduling + allocation cost of managing that state at scale (hundreds
of concurrent small flows) dwarfs the actual AEAD work.

Replacing this with a single TUN/L3 pipe (section 3 of the spec)
collapses N per-flow fds/goroutines/route-entries down to one IP-layer
channel: the tunnel's job becomes "read a packet from TUN, write a QUIC
datagram" and the reverse, with **no per-inner-flow socket, goroutine,
or route-table state on the tunnel's own transport layer at all**. Inner
TCP/UDP flow state now lives only in the guest OS's own IP stack (both
ends of the TUN), which is what a real OS does this well natively —
QUICochet no longer needs to reimplement per-flow connection tracking on
top of it.

See `docs/LOADTEST-BEFORE-AFTER.md` for the numeric before/after
comparison once the TUN/L3 mode is implemented.
