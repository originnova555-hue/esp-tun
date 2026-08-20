#!/usr/bin/env python3
"""Many-small-flows load generator for the TUN/L3 datapath.

Opens N concurrent plain TCP connections directly to a target
reachable through a TUN interface's routed subnet (no SOCKS5 handshake
— the TUN device carries the whole IP flow), each doing one small
HTTP GET/response, then closes and repeats for the given duration.
This is the TUN-mode equivalent of many_flows.py, used to produce a
before/after comparison against the SOCKS5 relay baseline.

Usage: many_flows_tun.py <target-ip> <target-port> [duration_sec=15] [concurrency=200]
"""
import socket
import threading
import time
import sys

target_ip = sys.argv[1]
target_port = int(sys.argv[2])
DURATION = float(sys.argv[3]) if len(sys.argv) > 3 else 15.0
CONCURRENCY = int(sys.argv[4]) if len(sys.argv) > 4 else 200

stop_at = time.time() + DURATION
ok_count = [0]
err_count = [0]
err_types = {}
lock = threading.Lock()


def one_flow():
    try:
        s = socket.create_connection((target_ip, target_port), timeout=5)
        s.sendall(b"GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
        data = s.recv(4096)
        s.close()
        with lock:
            ok_count[0] += 1
    except Exception as e:
        with lock:
            err_types.setdefault(type(e).__name__, 0)
            err_types[type(e).__name__] += 1
        with lock:
            err_count[0] += 1


def worker():
    while time.time() < stop_at:
        one_flow()


threads = [threading.Thread(target=worker, daemon=True) for _ in range(CONCURRENCY)]
start = time.time()
for t in threads:
    t.start()
for t in threads:
    t.join()
elapsed = time.time() - start

print(f"duration={elapsed:.1f}s concurrency={CONCURRENCY} ok={ok_count[0]} err={err_count[0]} flows/sec={(ok_count[0]+err_count[0])/elapsed:.1f}")
print("error breakdown:", err_types)
