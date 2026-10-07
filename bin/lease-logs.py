#!/usr/bin/env python3
#
# Read container logs and kubernetes events for the lease. The thing akash/README.md
# said was impossible ("the Console API has no logs endpoint") — it does not, but the
# PROVIDER does, and Console will mint the credential for it.
#
#   bin/lease-logs.py                       last 100 lines, every container
#   bin/lease-logs.py --service node        just the chain node
#   bin/lease-logs.py --tail 400            more history
#   bin/lease-logs.py --follow              stream until interrupted
#   bin/lease-logs.py --events              kubernetes events (BackOff, OOMKilled, Pulled…)
#
# Three things make this work, each learned by failing:
#
#   1. Auth is a JWT, not the API key. `POST /v1/create-jwt-token` with the API key
#      returns a token the provider accepts as `Authorization: Bearer`. The deployment
#      owner is a Console MANAGED WALLET, so we hold no key and cannot sign one
#      ourselves — Console signs it server side. Scope it to what you need; the token
#      is short lived (ttl seconds) and grants exactly the listed operations.
#
#   2. `logs` and `kubeevents` are WEBSOCKET endpoints. A plain GET returns
#      `400 Bad Request` with no explanation, which reads like a bad query string and
#      is not one. Only `status` is ordinary REST.
#
#   3. Nothing on this machine speaks websocket — no websocat, no wscat, no python
#      `websockets`, and node 18 has no global WebSocket. Hence the hand-rolled client
#      below: TLS, an Upgrade handshake, and a frame reader. Server frames are
#      unmasked, which is the only reason it stays this short.
#
# TLS IS VERIFIED (final audit BD-10). The bearer token is itself the secret: a
# `shell` token execs into the validator (consensus key) and the backend (hot wallet).
# So before the token is sent the provider must prove who it is, one of two ways:
#   1. a certificate that verifies against the system CAs for the provider's hostname
#      (the ADX providers used here serve Let's Encrypt certificates), or
#   2. the exact certificate the provider registered on chain (Akash's cert module,
#      read from AKASH_LCD over verified HTTPS), for a provider with a self-signed one.
# Anything else aborts before the Authorization header leaves this machine. Tokens are
# minted short (TTL_SECONDS; longer only for --follow) and scoped to the operations
# each tool needs.
#
# The `services=` query parameter is accepted and IGNORED by the provider; filtering
# happens here instead. Containers are named `node-0`, `edge-<hash>`, `relayer-<hash>`, `cloudflared-<hash>`,
# so `--service node` matches on prefix. Expect ~100 lines per container however large
# --tail is, and only the CURRENT container instance: a crash-looping container loses
# its previous run's output, so pull early.

import argparse
import base64
import json
import os
import socket
import ssl
import struct
import sys
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CONSOLE = "https://console-api.akash.network"
# Akash mainnet LCD, for the provider's on-chain certificates (fallback 2 above).
AKASH_LCD = os.environ.get("AKASH_LCD", "https://api.akashnet.net")
# The token is checked at the websocket handshake; a one-shot read needs seconds.
TTL_SECONDS = 300


def load_env():
    """AKASH_API_KEY and DSEQ live in the gitignored .env, same as for deploy.sh."""
    env = {}
    path = os.path.join(HERE, ".env")
    with open(path) as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip().strip('"').strip("'")
    for key in ("AKASH_API_KEY", "DSEQ"):
        if not env.get(key):
            sys.exit(f"{key} is not set in {path}")
    return env


def api(url, key, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method="POST" if data else "GET")
    req.add_header("x-api-key", key)
    # Cloudflare fronts console-api and 403s the default `Python-urllib/3.x` agent.
    # Nothing about the request is wrong; it never reaches the API.
    req.add_header("user-agent", "curl/8.7.1")
    if data:
        req.add_header("content-type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)


def mint_token(key, scope, ttl=TTL_SECONDS):
    body = {"data": {"ttl": ttl, "leases": {"access": "scoped", "scope": scope}}}
    return api(f"{CONSOLE}/v1/create-jwt-token", key, body)["data"]["token"]


def provider_host(key, dseq):
    """The lease says which provider ADDRESS holds it; the provider record says where.

    Returns (host, port, provider address); the address is what the on-chain
    certificate lookup in connect() needs."""
    dep = api(f"{CONSOLE}/v1/deployments/{dseq}", key)["data"]
    addr = dep["leases"][0]["id"]["provider"]
    host = api(f"{CONSOLE}/v1/providers/{addr}", key)
    host = host.get("data", host)["hostUri"]
    h, p = host.replace("https://", "").split(":")
    return h, p, addr


def onchain_certs(provider):
    """DER bytes of every VALID certificate the provider registered on chain."""
    ders, key = set(), ""
    while True:
        url = (f"{AKASH_LCD}/akash/cert/v1/certificates/list?filter.owner={provider}"
               f"&filter.state=valid&pagination.limit=100")
        if key:
            url += "&pagination.key=" + urllib.parse.quote(key)
        req = urllib.request.Request(url, headers={"user-agent": "curl/8.7.1"})
        with urllib.request.urlopen(req, timeout=30) as resp:   # verified TLS
            page = json.load(resp)
        for c in page.get("certificates", []):
            pem = base64.b64decode(c["certificate"]["cert"]).decode()
            ders.add(ssl.PEM_cert_to_DER_cert(pem))
        key = (page.get("pagination") or {}).get("next_key") or ""
        if not key:
            return ders


def connect(host, port, provider):
    """A TLS socket to the provider whose identity is verified (see the header)."""
    try:
        ctx = ssl.create_default_context()
        return ctx.wrap_socket(socket.create_connection((host, int(port)), timeout=60),
                               server_hostname=host)
    except ssl.SSLCertVerificationError as e:
        why = e.verify_message
    # Not publicly verifiable: accept only the provider's own on-chain certificate.
    ctx = ssl._create_unverified_context()
    sock = ctx.wrap_socket(socket.create_connection((host, int(port)), timeout=60),
                           server_hostname=host)
    der = sock.getpeercert(binary_form=True)
    if provider and der in onchain_certs(provider):
        return sock
    sock.close()
    sys.exit(f"refusing {host}:{port}: its certificate does not verify ({why}) and is "
             f"not one {provider} registered on chain. No token was sent.")


def ws_stream(host, port, path, token, on_line, provider=""):
    """Minimal RFC 6455 client: handshake, then read frames until the server closes."""
    key = base64.b64encode(os.urandom(16)).decode()
    req = (
        f"GET {path} HTTP/1.1\r\n"
        f"Host: {host}:{port}\r\n"
        "Upgrade: websocket\r\nConnection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
        f"Authorization: Bearer {token}\r\n\r\n"
    )
    sock = connect(host, port, provider)
    sock.settimeout(60)
    sock.sendall(req.encode())

    buf = b""
    while b"\r\n\r\n" not in buf:
        chunk = sock.recv(4096)
        if not chunk:
            sys.exit("provider closed the connection during the handshake")
        buf += chunk
    head, data = buf.split(b"\r\n\r\n", 1)
    if b" 101" not in head.split(b"\r\n")[0]:
        sys.exit(head.split(b"\r\n")[0].decode() + "\n" + data[:400].decode("utf-8", "replace"))

    def need(n):
        nonlocal data
        while len(data) < n:
            chunk = sock.recv(65536)
            if not chunk:
                raise EOFError
            data += chunk
        out, data = data[:n], data[n:]
        return out

    try:
        while True:
            hdr = need(2)
            opcode, length = hdr[0] & 0x0F, hdr[1] & 0x7F
            if length == 126:
                length = struct.unpack(">H", need(2))[0]
            elif length == 127:
                length = struct.unpack(">Q", need(8))[0]
            payload = need(length) if length else b""
            if opcode in (0, 1, 2):
                on_line(payload.decode("utf-8", "replace"))
            elif opcode == 8:          # close
                break
            elif opcode == 9:          # ping -> pong, masked as clients must
                sock.sendall(b"\x8a\x80" + os.urandom(4))
    except (EOFError, socket.timeout, KeyboardInterrupt):
        pass
    finally:
        sock.close()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--service", help="container name prefix, e.g. node / edge / relayer / cloudflared")
    ap.add_argument("--tail", type=int, default=100)
    ap.add_argument("--follow", action="store_true")
    ap.add_argument("--dseq", help="lease to target; defaults to DSEQ in .env")
    ap.add_argument("--events", action="store_true", help="kubernetes events instead of logs")
    args = ap.parse_args()

    env = load_env()
    dseq = args.dseq or env["DSEQ"]
    kind = "kubeevents" if args.events else "logs"
    # --follow keeps the stream open; the token is presented once, at the handshake.
    token = mint_token(env["AKASH_API_KEY"], ["status", "logs", "events"],
                       ttl=1800 if args.follow else TTL_SECONDS)
    host, port, provider = provider_host(env["AKASH_API_KEY"], dseq)

    follow = "true" if args.follow else "false"
    path = f"/lease/{dseq}/1/1/{kind}?follow={follow}"
    if not args.events:
        path += f"&tail={args.tail}"

    def emit(text):
        # One websocket frame can carry several newline-separated JSON objects.
        for line in text.splitlines():
            if not line.strip():
                continue
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                print(line)
                continue
            if args.events:
                obj = msg.get("object", {})
                print(f"{msg.get('type'):8} {msg.get('reason'):18} "
                      f"{obj.get('name','')}  {msg.get('note','')}")
                continue
            name = msg.get("name", "")
            if args.service and not name.startswith(args.service):
                continue
            print(f"{name}  {msg.get('message','')}")

    ws_stream(host, port, path, token, emit, provider)


if __name__ == "__main__":
    main()
