#!/usr/bin/env python3
"""Check from outside that rpc.* and lcd.* reach earth-edge, not the node.

    bin/check-edge.py                                   # rpc/lcd.erth.network
    bin/check-edge.py --rpc https://rpc.example --lcd https://lcd.example

Exit 0 when every check passes, 1 otherwise (one line per failure), so it
can run from cron or any alerting job (round-5 R5-E-8). Run it after every
deploy, relaunch and tunnel change, and on a schedule: Public Hostnames are
configured at Cloudflare per tunnel, so a relaunch that reuses a tunnel
keeps whatever ingress it had, `http://node:26657` included, and that serves
the public unfiltered at once. Nothing in the SDL can see it.

The tell is the X-Earth-Edge header: earth-edge sets it on every answer,
served or refused, and the node never does. On top of that, a few calls
the edge refuses and the node would serve must come back refused.

Standard library only; sends nothing but public requests.
"""
import argparse
import json
import sys
import urllib.error
import urllib.request

# The fee collector: every tx pays it, so a search on it is the whole history
# (R5-E-1). The node would run it; the edge refuses it.
FEE_COLLECTOR = "earth17xpfvakm2amg962yls6f84z3kell8c5lthcx95"
UA = "earth-check-edge/1"


def fetch(url, body=None):
    req = urllib.request.Request(url, data=body, headers={"User-Agent": UA})
    if body is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, r.headers, r.read(4096)
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read(4096)
    except Exception as e:  # network, TLS, DNS
        return None, {}, str(e).encode()


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--rpc", default="https://rpc.erth.network")
    ap.add_argument("--lcd", default="https://lcd.erth.network")
    a = ap.parse_args()
    rpc, lcd = a.rpc.rstrip("/"), a.lcd.rstrip("/")
    simulate = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "abci_query",
                           "params": {"path": "/cosmos.tx.v1beta1.Service/Simulate", "data": "00"}}).encode()
    checks = [
        # (what, url, body, want: "served" or "refused")
        ("rpc status", rpc + "/status", None, "served"),
        ("rpc tx_search", rpc + '/tx_search?query="tx.height>0"', None, "refused"),
        ("rpc abci_query Simulate", rpc + "/", simulate, "refused"),
        ("lcd syncing", lcd + "/cosmos/base/tendermint/v1beta1/syncing", None, "served"),
        ("lcd fee-collector search", lcd + "/cosmos/tx/v1beta1/txs?query=transfer.recipient%3D%27"
         + FEE_COLLECTOR + "%27&limit=1", None, "refused"),
        ("lcd count_total", lcd + "/cosmos/staking/v1beta1/validators?pagination.count_total=true", None, "refused"),
    ]
    failed = 0
    for what, url, body, want in checks:
        code, headers, text = fetch(url, body)
        marked = headers.get("X-Earth-Edge") == "1" if headers else False
        problems = []
        if code is None:
            problems.append("no answer (%s)" % text.decode(errors="replace")[:120])
        else:
            if not marked:
                problems.append("no X-Earth-Edge header: this hostname does not reach earth-edge "
                                "(a Public Hostname pointing at node:*?)")
            if want == "served" and code != 200:
                problems.append("HTTP %d, want 200" % code)
            if want == "refused" and (code == 200 or b"refused by the edge filter" not in text):
                problems.append("HTTP %d and not refused by the edge filter: the node answered it" % code)
        if problems:
            failed += 1
            print("FAIL %-26s %s" % (what, "; ".join(problems)))
        else:
            print("ok   %-26s HTTP %d" % (what, code))
    if failed:
        print("%d of %d checks failed" % (failed, len(checks)))
        sys.exit(1)


if __name__ == "__main__":
    main()
