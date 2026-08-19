# Congestion control

`quic.congestion_control` accepts `cubic` and `auto`. It also accepts `bbrv1`,
but **this build does not implement BBR**: the config loader warns and falls
back to Cubic rather than silently doing something other than what you asked.

## Why bbrv1 is not available

The QUIC stack this engine is built on (quic-go) ships exactly one sender —
Cubic, in `internal/congestion` — and offers no hook to substitute another.
Adding BBRv1 means vendoring a patched quic-go with a bandwidth sampler,
windowed min-RTT filter and the full state machine, and then validating it.
That is a real piece of work and it is tracked separately; it is not something
that can be turned on with a config key today.

## Why this matters less than it looks

The datapath carries payload in QUIC **DATAGRAM** frames (RFC 9221), never in
streams. Datagrams are not retransmitted, so the tunnel never stacks its own
reliability on top of the inner flow's. What the outer congestion controller
still does is gate *when* datagrams may leave.

For a layer-3 tunnel, that outer gate is a mixed blessing. The traffic inside
the tunnel is overwhelmingly TCP, and TCP already runs its own congestion
control end to end. An aggressive outer controller reacting to the same loss
event the inner flows are reacting to makes both back off at once, which is one
of the classic ways a tunnel collapses under load. WireGuard's answer is to
have no outer congestion control at all.

So the practical ordering for this workload is:

1. Cubic with a healthy send buffer — what you get today, and what the tiers set.
2. BBRv1 — better on paths with shallow buffers and non-congestive loss, which
   is the usual case on an intercontinental path with active interference.
3. Pacing-only — arguably the best fit for L3, and the cheapest to implement of
   the three, but it needs the same vendoring work as BBR to reach the sender.

If you are seeing throughput collapse rather than steady-state slowness, look
first at obfuscation padding and at the buffer sizes, not at the CC algorithm.
