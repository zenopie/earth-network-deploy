#!/usr/bin/env bash
#
# Check the relaunch genesis before deploying the launch tag anywhere.
#
#   bin/check-genesis.sh <tag>                       release asset vs the pin
#   bin/check-genesis.sh <tag> --chain <chain repo>  ...and vs that checkout
#   bin/check-genesis.sh --chain <chain repo>        checkout vs the pin only
#
# The pin is akash/genesis.sha256: the sha256 of the chain repo's
# networks/genesis.json for the privacy relaunch. It is written by hand once the
# genesis is final, which makes "the genesis we meant to launch" a reviewed line
# in this repo rather than whatever the image happens to contain.
#
# What this cannot check: the copy baked into the image (/etc/earth/genesis.json).
# The entrypoint checks that one against the .sha256 baked beside it and refuses
# to start on a mismatch -- and with RESET_ON_GENESIS_MISMATCH=1, a volume whose
# genesis differs from the image's is WIPED. So the image must carry exactly the
# file this script approved: build the launch tag from the commit whose
# networks/genesis.json hashes to the pin.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TAG=""; CHAIN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --chain) CHAIN="${2:?--chain needs a path}"; shift 2 ;;
    -*) echo "unknown argument: $1" >&2; exit 2 ;;
    *) TAG="$1"; shift ;;
  esac
done
[ -n "$TAG$CHAIN" ] || { echo "usage: check-genesis.sh [<tag>] [--chain <chain repo>]" >&2; exit 2; }

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
  python3 - "$2" "$WANT_CHAIN_ID" <<'PY' || FAIL=1
import json, sys, datetime
g = json.load(open(sys.argv[1]))
cid, t = g["chain_id"], g["genesis_time"]
print("      chain_id %s  genesis_time %s" % (cid, t))
ok = True
if cid != sys.argv[2]:
    print("FAIL  chain_id %s, but akash/deploy.yaml says CHAIN_ID=%s" % (cid, sys.argv[2])); ok = False
when = datetime.datetime.fromisoformat(t.replace("Z", "+00:00"))
if when < datetime.datetime.now(datetime.timezone.utc):
    # Emission and the POL burn are prorated from genesis_time: a past time
    # pays the whole gap out at height 2 (networks/genesis/chain.json).
    print("WARN  genesis_time is in the past; fine only for a chain that is already running")
gt = g.get("app_state", {}).get("genutil", {}).get("gen_txs", [])
print("      gentxs %d" % len(gt))
if not gt:
    print("FAIL  no gentx: the chain would have no validator at height 1"); ok = False
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
