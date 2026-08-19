# The spoofed UDP transport

Everything the tunnel sends leaves with a source address that does not belong
to the machine sending it. That address is one the censor's filter already
lets through, which is what makes the tunnel work during a layer-3 blackout.

There are two ways to forge a source address on Linux, and the choice between
them decides the throughput ceiling of the whole project.

## What not to do: a raw socket with IP_HDRINCL

The obvious approach — open `SOCK_RAW`, set `IP_HDRINCL`, build the IP header
yourself, compute the checksum yourself — works, and it is what the earlier
minimal core did. It also costs you the entire kernel fast path:

- **No segmentation offload.** Every packet is its own `sendto`. A 64 KB write
  that could have gone out as one syscall and been segmented by the NIC becomes
  45 syscalls.
- **No checksum offload.** The IP checksum is computed in Go, per packet, on
  the CPU that could have been doing crypto.
- **No GRO on the way back**, and no batching primitives that fit.

At 1300-byte packets, 600 Mbps is about 58,000 packets per second in each
direction. Paying a syscall and a software checksum for each of them is the
single biggest reason a tunnel built this way plateaus early and burns CPU
doing it.

## What this does instead: normal UDP, source overridden per packet

The socket is an ordinary `SOCK_DGRAM` bound to a wildcard address. Two things
make it forge sources:

1. **`IP_TRANSPARENT`** on the socket, which is what makes the kernel willing
   to route a packet whose source is not local. It needs `CAP_NET_ADMIN`.
2. **An `IP_PKTINFO` control message on every `sendmsg`**, whose `ipi_spec_dst`
   field the kernel takes as the outgoing source address.

The kernel still builds the IP header, still computes the checksum in hardware,
still segments large writes, and still accepts batched `sendmmsg`/`recvmmsg`.
The only thing that changed is four bytes of a control message.

## Keeping the QUIC layer's fast path

The QUIC stack has its own optimised socket path — `sendmmsg` with
`UDP_SEGMENT` outbound, `recvmmsg` inbound — but it only uses it for a
connection that satisfies two interfaces: `quic.OOBCapablePacketConn`, and an
unexported batched-read shape. A plain wrapper type would fail both and fall
back to one packet per syscall, silently.

So `transport.Conn` implements both, and `internal/transport/interface_linux.go`
asserts it at compile time. The write path is the interesting one: the QUIC
layer hands us a control-message buffer that *already* contains a PKTINFO entry
(because the socket is wildcard-bound) plus, when offload is in play, a
`UDP_SEGMENT` entry. We scan it, rewrite four bytes in place, and pass it
straight through:

```
BenchmarkPatchPktinfo-4   258332974   4.734 ns/op   0 B/op   0 allocs/op
```

Under 5 ns and no allocation per packet. At 60,000 packets per second that is
about 0.03% of one core — the spoofing itself is free, and all the throughput
work belongs to layers above.

The rare case — no PKTINFO in the buffer, which happens on a connection's first
packets — builds a fresh control message into a pooled scratch buffer.

## Why UDP_GRO is off

`transport.gro` controls *batched reads* (`recvmmsg`), which is what the
`ReadBatch` method provides. Kernel-side `UDP_GRO`, which coalesces several
received datagrams into one, is deliberately **not** enabled: the QUIC layer
has no way to split a coalesced read back into the datagrams it was made from,
so switching it on would corrupt every packet received.

## Socket buffers

Buffers are sized from config with `SO_RCVBUFFORCE` / `SO_SNDBUFFORCE`, which
bypass `net.core.rmem_max`. Without `CAP_NET_ADMIN` the ordinary options are
used and the startup log reports what the kernel actually granted. This is
worth caring about: an undersized receive buffer shows up as bursty loss under
load, and every layer above misreads that as congestion.

## The spoof-source pool

There is no way to probe a forged source directly — nothing will tell you that
an address has quietly started being dropped. The liveness test that is
actually available is *"are we still hearing from the peer while we are
talking to it"*, so that is what `internal/spoof` watches:

- Silence only counts when packets were actually sent in the interval. An idle
  tunnel is quiet for entirely ordinary reasons and never triggers a rotation.
- After `fail_threshold` consecutive silent intervals the active address is
  quarantined for `quarantine_sec` and the pool rotates to the next healthy one.
- If every address is quarantined, the pool picks the one that recovers
  soonest. Sending from a suspect address beats sending from none.

Rotation fires a callback so the session layer can redial: the existing QUIC
connections are talking to an address the peer has stopped hearing, and they
will not recover on their own.
