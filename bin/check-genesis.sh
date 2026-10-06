#!/usr/bin/env bash
#
# Check the relaunch genesis before deploying the launch tag anywhere.
#
#   bin/check-genesis.sh <tag>                       release asset vs the pin
#   bin/check-genesis.sh <tag> --chain <chain repo>  ...and vs that checkout
#   bin/check-genesis.sh --chain <chain repo>        checkout vs the pin only
#   ... --allow-placeholder                          accept a pre-ceremony genesis
#
# The pin is akash/genesis.sha256: the sha256 of the chain repo's
# networks/genesis.json for the privacy relaunch. It is written by hand once the
# genesis is final, which makes "the genesis we meant to launch" a reviewed line
# in this repo rather than whatever the image happens to contain.
#
# A genesis passes only if it is post-ceremony (final audit BD-8): genesis_time
# in the future, one gentx from the operator earth1n6amv… with the launch
# consensus key PGqvPN4C…, and none of the placeholder validator's or devnet
# accounts. --allow-placeholder turns those into WARNs, for checking the
# pre-ceremony pin; never use it on the launch tag.
#
# What this cannot check: the copy baked into the image (/etc/earth/genesis.json).
# The entrypoint checks that one against the .sha256 baked beside it and refuses
# to start on a mismatch, and on a resumed volume refuses a genesis that differs
# from the image's. So the image must carry exactly the file this script
# approved: build the launch tag from the commit whose networks/genesis.json
# hashes to the pin.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TAG=""; CHAIN=""; ALLOW_PLACEHOLDER=0
while [ $# -gt 0 ]; do
  case "$1" in
    --chain) CHAIN="${2:?--chain needs a path}"; shift 2 ;;
    --allow-placeholder) ALLOW_PLACEHOLDER=1; shift ;;
    -*) echo "unknown argument: $1" >&2; exit 2 ;;
    *) TAG="$1"; shift ;;
  esac
done
[ -n "$TAG$CHAIN" ] || { echo "usage: check-genesis.sh [<tag>] [--chain <chain repo>] [--allow-placeholder]" >&2; exit 2; }

PIN="$(awk '{print $1}' "$HERE/akash/genesis.sha256")"
case "$PIN" in
  TODO*) echo "akash/genesis.sha256 is still a TODO: pin the final genesis sha256 first" >&2; exit 1 ;;
esac
[[ "$PIN" =~ ^[0-9a-f]{64}$ ]] || { echo "akash/genesis.sha256 is not a sha256: $PIN" >&2; exit 1; }

sha() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'; }
WANT_CHAIN_ID="$(sed -n 's/^ *- CHAIN_ID=//p' "$HERE/akash/deploy.yaml" | head -1)"
FAIL=0

check() {  # <label> <file>
  local got; got="$(sha "$2")"
  if [ "$got" = "$PIN" ]; then echo "ok    $1  $got"; else echo "FAIL  $1  $got (pin $PIN)"; FAIL=1; fi
  python3 - "$2" "$WANT_CHAIN_ID" "$ALLOW_PLACEHOLDER" <<'PY' || FAIL=1
import json, sys, datetime
# The ceremony's facts (chain repo scripts/ceremony.sh; RELAUNCH.md).
OPERATOR = "earth1n6amvkgfrrgy6ulhurewnm0endkgye69fkcapr"
CONSENSUS_PUBKEY = "PGqvPN4CxEkxvvh3tSBX0SGeBgjMdqQwZkdHt8FRLm4="
PLACEHOLDERS = {
    "earth14e6sqtf5y7mtzwykqreewe9kg3w94t0f25d54a": "placeholder validator",
    "earth1s7rgscltvw8v3kzhj46pptdqg843ngs7th9ywp": "devnet faucet",
    "earth1jtc2zjmmmyttdayz6aw8vfgt5qn4hg7rpxaar6": "old gas wallet",
}
CS = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
def b32_data(addr):
    """bech32 -> payload bytes (checksum verified)."""
    hrp, data = addr.rsplit("1", 1)
    v = [CS.index(c) for c in data]
    def polymod(vals):
        g = [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3]; chk = 1
        for x in vals:
            b = chk >> 25; chk = (chk & 0x1ffffff) << 5 ^ x
            for i in range(5):
                chk ^= g[i] if (b >> i) & 1 else 0
        return chk
    assert polymod([ord(c) >> 5 for c in hrp] + [0] + [ord(c) & 31 for c in hrp] + v) == 1, addr
    acc = bits = 0; out = []
    for x in v[:-6]:
        acc = acc << 5 | x; bits += 5
        while bits >= 8:
            bits -= 8; out.append(acc >> bits & 0xff)
    return bytes(out)

g = json.load(open(sys.argv[1]))
allow = sys.argv[3] == "1"
cid, t = g["chain_id"], g["genesis_time"]
print("      chain_id %s  genesis_time %s" % (cid, t))
ok = True
def placeholder(msg):
    # Pre-ceremony facts: FAIL unless --allow-placeholder.
    global ok
    if allow:
        print("WARN  " + msg + " (--allow-placeholder)")
    else:
        print("FAIL  " + msg); ok = False
if cid != sys.argv[2]:
    print("FAIL  chain_id %s, but akash/deploy.yaml says CHAIN_ID=%s" % (cid, sys.argv[2])); ok = False
when = datetime.datetime.fromisoformat(t.replace("Z", "+00:00"))
if when < datetime.datetime.now(datetime.timezone.utc):
    # Emission and the POL burn are prorated from genesis_time: a past time
    # pays the whole gap out at height 2 (networks/genesis/chain.json).
    placeholder("genesis_time %s is in the past: a pre-ceremony genesis" % t)
gt = g.get("app_state", {}).get("genutil", {}).get("gen_txs", [])
print("      gentxs %d" % len(gt))
if not gt:
    print("FAIL  no gentx: the chain would have no validator at height 1"); ok = False
for tx in gt:
    for m in tx["body"]["messages"]:
        val = m.get("validator_address", "")
        pub = m.get("pubkey", {}).get("key", "")
        if pub != CONSENSUS_PUBKEY:
            print("FAIL  gentx consensus pubkey %s, launch key is %s" % (pub, CONSENSUS_PUBKEY)); ok = False
        if not val or b32_data(val) != b32_data(OPERATOR):
            placeholder("gentx operator %s is not %s's" % (val, OPERATOR))
raw = json.dumps(g)
for addr, what in PLACEHOLDERS.items():
    if addr in raw or addr[len("earth1"):-6] in raw:
        placeholder("genesis still carries the %s %s…" % (what, addr[:12]))
sys.exit(0 if ok else 1)
PY
}

if [ -n "$CHAIN" ]; then
  check "chain checkout networks/genesis.json" "$CHAIN/networks/genesis.json"
  committed="$(awk '{print $1}' "$CHAIN/networks/genesis.json.sha256")"
  [ "$committed" = "$PIN" ] && echo "ok    networks/genesis.json.sha256 agrees" \
    || { echo "FAIL  networks/genesis.json.sha256 says $committed"; FAIL=1; }
fi

if [ -n "$TAG" ]; then
  WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
  URL="https://github.com/zenopie/earth-network-chain/releases/download/$TAG/genesis.json"
  curl -fsSL -o "$WORK/genesis.json" "$URL" || { echo "FAIL  could not fetch $URL" >&2; exit 1; }
  check "release $TAG genesis.json" "$WORK/genesis.json"
fi

[ "$FAIL" = 0 ] && echo "genesis checks passed" || { echo "genesis checks FAILED" >&2; exit 1; }
