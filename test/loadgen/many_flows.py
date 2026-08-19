#!/usr/bin/env python3
"""Many-small-flows load generator for the SOCKS5 relay baseline.

Opens N concurrent SOCKS5 connections against a running quiccochet
client's inbound, each doing a full SOCKS5 handshake plus one small
HTTP GET/response, then closes and repeats for the given duration.
Simulates the "hundreds of small concurrent flows (DNS, HTTP
keep-alive, WebRTC)" scenario, not a single bulk iperf3 stream.

Usage: many_flows.py [duration_sec=15] [concurrency=200]
Requires a target HTTP server reachable through the SOCKS5 proxy.
"""
import socket
import struct
import threading
import time
import sys

SOCKS_HOST = "127.0.0.1"
SOCKS_PORT = 11080
TARGET_HOST = "127.0.0.1"
TARGET_PORT = 19999
DURATION = float(sys.argv[1]) if len(sys.argv) > 1 else 15.0
CONCURRENCY = int(sys.argv[2]) if len(sys.argv) > 2 else 200

stop_at = time.time() + DURATION
ok_count = [0]
err_count = [0]
lock = threading.Lock()

def one_flow():
    try:
        s = socket.create_connection((SOCKS_HOST, SOCKS_PORT), timeout=5)
        s.sendall(b"\x05\x01\x00")  # no auth
        resp = s.recv(2)
        # CONNECT request
        host_b = TARGET_HOST.encode()
        req = b"\x05\x01\x00\x03" + bytes([len(host_b)]) + host_b + struct.pack(">H", TARGET_PORT)
        s.sendall(req)
        resp = s.recv(10)
        if resp[1] != 0x00:
            with lock:
                err_count[0] += 1
            s.close()
            return
        # small HTTP request (simulates DNS/HTTP keep-alive/WebRTC-sized small flow)
        s.sendall(b"GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
        data = s.recv(4096)
        s.close()
        with lock:
            ok_count[0] += 1
    except Exception as e:
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

print(f"duration={elapsed:.1f}s concurrency={CONCURRENCY} ok={ok_count[0]} err={err_count[0]} flows/sec={ (ok_count[0]+err_count[0])/elapsed:.1f}")
