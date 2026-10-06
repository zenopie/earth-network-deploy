#!/usr/bin/env bash
#
# Create a NEW lease for the node, and print the DSEQ it got.
#
#   FLAGS="--fullnode --no-statesync --validator-key --node-key --tunnel"
#   bin/create.sh <tag> $FLAGS --var DSEQ                   cheapest bid, ask first
#   bin/create.sh <tag> $FLAGS --var DSEQ --provider akash1..  that provider's bid
#   bin/create.sh <tag> $FLAGS --var DSEQ --yes             do not ask
#   bin/create.sh <tag> $FLAGS --sdl-only                   build and validate, send nothing
#
# With --validator-key it refuses a second validator lease for the same
# genesis (akash/validator-lease.lock, the Console API); see "one validator
# lease per genesis" below for the checks and the one override.
#
# The flags are the validator's (RELAUNCH.md, section 3); build-sdl.py refuses
# a build without --fullnode.
#
# This is NOT deploy.sh. deploy.sh updates a running deployment in place with
# PUT, which keeps the volumes and therefore the chain's height and history.
# There is no in-place path from "no lease" to "a lease", so this exists for the
# two occasions that need one: the first launch, and a close-and-recreate after
# a structural change (endpoint kinds and resources are part of what a provider
# bid on, so PUT rejects them).
#
# A new lease means a NEW VOLUME. The chain starts at height 1 from the genesis
# baked into the image. Every account, every registration and all history from
# the previous lease is gone — that is what closing it did, and this cannot undo
# it. build-sdl.py refuses to build an SDL that would come up without a signer,
# which is the one way this goes wrong quietly.
#
# Afterwards, two things still need doing and neither is automatic:
#
#   1. EXTERNAL_ADDRESS in akash/deploy.yaml is the p2p address peers dial, and
#      the provider assigns the port, so the old lease's value is now wrong.
#      It is not urgent while this is the only node: the tunnel carries lcd and
#      rpc, p2p is the one thing on a provider port because CometBFT's protocol
#      is not HTTP and cloudflared could not usefully proxy it. A stale value
#      only means the node is not connectable *inbound* — it still dials out.
#      It matters the day a second node wants to join.
#   2. The ads-for-gas backend has its own lease and its own EARTH_NODE_URL.
#      Point it at the tunnel (https://lcd.erth.network), never at a provider
#      hostname — from inside the same provider's cluster that is a hairpin that
#      hangs rather than fails.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
API=https://console-api.akash.network/v1

TAG="${1:?usage: create.sh <tag> [--provider <addr>] [--deposit <akt>] [--yes] [--sdl-only] [--fullnode] [--no-statesync] [--validator-key] [--node-key] [--tunnel] [--relayer] [--var NAME] [--genesis <file>] [--replace-validator-lease <closed DSEQ>]}"
shift

PROVIDER=""; DEPOSIT=5; ASSUME_YES=0; SDL_ONLY=0; SDL_FILE="akash/deploy.yaml"; FULLNODE=0
NO_STATESYNC=0; DSEQ_VAR=""; GENESIS_FILE=""; REPLACE_DSEQ=""
while [ $# -gt 0 ]; do
  case "$1" in
    --provider) PROVIDER="${2:?--provider needs an address}"; shift 2 ;;
    --deposit)  DEPOSIT="${2:?--deposit needs a number}"; shift 2 ;;
    --yes)      ASSUME_YES=1; shift ;;
    --sdl-only) SDL_ONLY=1; shift ;;
    --sdl)      SDL_FILE="${2:?--sdl needs a path}"; shift 2 ;;
    --fullnode) FULLNODE=1; shift ;;
    --no-statesync) NO_STATESYNC=1; shift ;;
    --validator-key) VALIDATOR_KEY=1; shift ;;
    --tunnel)   TUNNEL=1; shift ;;
    --node-key) NODE_KEY=1; shift ;;
    --relayer)  RELAYER=1; shift ;;
    --var)      DSEQ_VAR="${2:?--var needs a name}"; shift 2 ;;
    --genesis)  GENESIS_FILE="${2:?--genesis needs a file}"; shift 2 ;;
    --replace-validator-lease) REPLACE_DSEQ="${2:?--replace-validator-lease needs the closed DSEQ}"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

# --fullnode keeps the mnemonics out of the SDL (see build-sdl.py). It also
# decides where the new DSEQ is written in .env, a leftover of the 2026-09-01
# lease migration, when a --fullnode lease was a SECOND node next to a live
# validator:
#
#   - without --fullnode, DSEQ (but build-sdl.py now refuses that build);
#   - with --fullnode, SYNC_DSEQ, which nothing here reads, so DSEQ -- the
#     handle deploy.sh, lease-logs.py and lease-shell.py use -- keeps pointing
#     at the previous lease;
#   - with --var NAME, NAME. The validator is created with --var DSEQ
#     (RELAUNCH.md, section 3), which is right whenever no other lease is live.
BUILD_ARGS=()
if [ "$FULLNODE" = 1 ]; then
  BUILD_ARGS+=(--fullnode)
fi
if [ "$NO_STATESYNC" = 1 ]; then
  BUILD_ARGS+=(--no-statesync)
fi
# --validator-key and --tunnel go straight through to build-sdl.py, as in
# deploy.sh. A lease that has to sign from its first boot must be created with
# them: adding them later is a PUT, and a pod that is not yet Ready (CometBFT
# opens no listener before genesis_time) may never be replaced by one.
if [ "${VALIDATOR_KEY:-0}" = 1 ]; then
  BUILD_ARGS+=(--validator-key)
fi
if [ "${TUNNEL:-0}" = 1 ]; then
  BUILD_ARGS+=(--tunnel)
fi
# --node-key: the validator's fixed p2p id on a NEW lease (privacy relaunch), so
# joiners can be told whom to dial before launch. Straight through to
# build-sdl.py.
if [ "${NODE_KEY:-0}" = 1 ]; then
  BUILD_ARGS+=(--node-key)
fi
# --relayer: inject RELAYER_MNEMONIC; build-sdl.py refuses it unless the
# relayer's ENABLED=true.
if [ "${RELAYER:-0}" = 1 ]; then
  BUILD_ARGS+=(--relayer)
fi
# --genesis: build-sdl.py checks the file hashes to the pin and its gentx names
# this key. Required with --replace-validator-lease (its genesis_time decides).
if [ -n "$GENESIS_FILE" ]; then
  BUILD_ARGS+=(--genesis "$GENESIS_FILE")
fi
[ -z "$REPLACE_DSEQ" ] || [ "${VALIDATOR_KEY:-0}" = 1 ] || {
  echo "--replace-validator-lease only applies with --validator-key" >&2; exit 2; }

[ -f "$HERE/.env" ] || { echo "no .env — it holds the secrets injected into the submitted SDL" >&2; exit 1; }
set -a; . "$HERE/.env"; set +a
: "${AKASH_API_KEY:?set AKASH_API_KEY in .env}"

# Everything the API returns is kept here rather than piped through, so a
# surprise in a response shape can be read afterwards instead of being lost with
# the pipeline that failed on it. It holds the SDL, which holds the secrets.
WORK="$(mktemp -d)"; chmod 700 "$WORK"; trap 'rm -rf "$WORK"' EXIT

# The API echoes the manifest back, and the manifest carries every secret that
# was injected into the SDL — the tunnel token, the validator mnemonic, the
# consensus key. So a failure prints a redacted body, never the raw one. This
# was learned the hard way: an unexpected status code dumped the token to a
# terminal, and it had to be rotated.
show_response() {
  python3 - "$1" <<'REDACT'
import json, re, sys
raw = open(sys.argv[1]).read()
raw = re.sub(r'((?:TUNNEL_TOKEN|MNEMONIC|PRIV_VALIDATOR_KEY_B64|NODE_KEY_B64|API_KEY)[^\s",]*=)[^"\\,\s]+',
             r'\1<redacted>', raw)
try:
    d = json.loads(raw)
    if isinstance(d, dict):
        for k in ("manifest", "sdl"):
            if k in d.get("data", {}): d["data"][k] = "<redacted>"
            if k in d: d[k] = "<redacted>"
    raw = json.dumps(d)
except Exception:
    pass
sys.stderr.write(raw[:800] + "\n")
REDACT
}

# --- one validator lease per genesis (R2-BD-3) -------------------------------
# A second lease holding the consensus key starts from height 1 of the image
# genesis with no peers: it re-signs heights the live chain already signed
# (double-sign evidence against the only validator, a fork every wallet sees)
# and attaches a second connector to the tunnel, so Cloudflare splits rpc/lcd
# between two chains (RELAUNCH.md section 9). So a --validator-key create is
# refused when:
#
#   - akash/validator-lease.lock (written below after the lease is taken, and
#     committed) records a validator lease for the SAME genesis pin. A real
#     relaunch has a new genesis_time, so a new pin, and passes; or
#   - the Console API reports the lock's DSEQ, or .env's DSEQ, as active (a
#     validator, or anything else on the tunnel, still running); or
#   - the Console API cannot be read (fail closed; try again).
#
# The one deliberate exception is a validator lease that died BEFORE
# genesis_time (nothing signed yet) being recreated for the same genesis:
#   --replace-validator-lease <its DSEQ> --genesis <the pinned genesis.json>
# The DSEQ must be the lock's, the Console must show it closed, and the
# genesis_time must be at least 15 minutes ahead. Past genesis_time there is
# no override: the key may have signed, and starting over is a new genesis and
# a new consensus key (RELAUNCH.md section 9), decided by a human.
LOCK="$HERE/akash/validator-lease.lock"
if [ "${VALIDATOR_KEY:-0}" = 1 ]; then
  python3 - "$LOCK" "$HERE/akash/genesis.sha256" "${DSEQ:-}" "$REPLACE_DSEQ" "$GENESIS_FILE" <<'GUARD'
import datetime, json, os, re, sys, urllib.error, urllib.request
lock_path, pin_path, env_dseq, replace, genesis = sys.argv[1:6]
pin = open(pin_path).read().split()[0]
lock = json.load(open(lock_path)) if os.path.exists(lock_path) else None
key = os.environ["AKASH_API_KEY"]

def die(msg):
    sys.exit("REFUSED: " + msg)

def state(dseq):
    """'active', 'closed' or 'absent' per the Console API; dies if unreadable."""
    req = urllib.request.Request(f"https://console-api.akash.network/v1/deployments/{dseq}",
                                 headers={"x-api-key": key})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            d = json.load(r)
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return "absent"
        die(f"Console API answered {e.code} for deployment {dseq}; cannot prove it is closed")
    except Exception as e:
        die(f"Console API unreadable for deployment {dseq} ({type(e).__name__}); cannot prove it is closed")
    d = d.get("data", d)
    dep = d.get("deployment", {})
    states = [str(dep.get("state", "")).lower()] + [str(l.get("state", "")).lower() for l in d.get("leases") or []]
    if "active" in states:
        return "active"
    if states[0] == "closed":
        return "closed"
    die(f"deployment {dseq}: unrecognised state {states}; cannot prove it is closed")

for dseq, what in ((lock and lock.get("dseq"), "the validator lease in akash/validator-lease.lock"),
                   (env_dseq, ".env's DSEQ")):
    if dseq and state(dseq) == "active":
        die(f"{what} ({dseq}) is active. One validator, one connector per tunnel: "
            "update it in place with deploy.sh, or close it first (RELAUNCH.md section 9).")

if lock and lock.get("genesis_sha256") == pin:
    if not replace:
        die(f"a validator lease ({lock.get('dseq')}, {lock.get('created')}) was already created for "
            f"genesis {pin[:12]}…. A second one replays heights 1.. with the launch key. If that lease "
            "died before genesis_time, see --replace-validator-lease in bin/create.sh; otherwise "
            "this is RELAUNCH.md section 9 (a new genesis and a new key), not a create.")
    if replace != str(lock.get("dseq")):
        die(f"--replace-validator-lease {replace} is not the locked lease {lock.get('dseq')}")
    if not genesis:
        die("--replace-validator-lease needs --genesis <the pinned genesis.json>: its genesis_time decides")
    gt = json.load(open(genesis))["genesis_time"]
    m = re.fullmatch(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d+)?Z", gt)
    if not m:
        die(f"genesis_time {gt!r} is not a UTC RFC 3339 time")
    t = datetime.datetime.fromisoformat(m.group(1)).replace(tzinfo=datetime.timezone.utc)
    left = (t - datetime.datetime.now(datetime.timezone.utc)).total_seconds()
    if left < 900:
        die(f"genesis_time {gt} is {'past' if left < 0 else 'under 15 minutes away'}: the key may "
            "have signed. No override: RELAUNCH.md section 9.")
    print(f"replacing validator lease {replace} (closed) for genesis {pin[:12]}…; genesis_time in {int(left)} s")
GUARD
fi

DIGEST="$("$HERE/bin/digest.sh" "$TAG")"
python3 "$HERE/bin/build-sdl.py" "$HERE" "$WORK/sdl.yaml" "$DIGEST" \
  --sdl "$SDL_FILE" ${BUILD_ARGS[@]+"${BUILD_ARGS[@]}"}

if [ "$SDL_ONLY" = 1 ]; then
  echo "sdl built and validated; nothing submitted"
  exit 0
fi

# --- 1. create the deployment ------------------------------------------------
# The SDL goes in as a JSON string, so it is written by a JSON encoder rather
# than interpolated: it is YAML full of quotes, colons and a base64 key.
python3 -c "
import json,sys
json.dump({'data':{'sdl':open(sys.argv[1]).read(),'deposit':int(sys.argv[2])}}, open(sys.argv[3],'w'))
" "$WORK/sdl.yaml" "$DEPOSIT" "$WORK/create.json"

CODE=$(curl -sS -m 300 -X POST \
  -H "x-api-key: ${AKASH_API_KEY}" -H 'content-type: application/json' \
  --data-binary @"$WORK/create.json" \
  "$API/deployments" -o "$WORK/created.json" -w '%{http_code}')
# 2xx, not 200: creating returns 201 and taking the lease returns 200, and
# rejecting 201 aborted a deployment that had in fact been created and paid for.
case "$CODE" in 2??) ;; *) echo "create failed (http $CODE)" >&2; show_response "$WORK/created.json"; exit 1 ;; esac

# dseq and manifest are read tolerantly: the response has been seen both bare
# and wrapped in {"data": ...}, and guessing wrong here loses a deployment that
# has already been paid for.
eval "$(python3 - "$WORK/created.json" <<'PY'
import json, sys, shlex
d = json.load(open(sys.argv[1]))
def dig(o, key):
    if isinstance(o, dict):
        if key in o: return o[key]
        for v in o.values():
            r = dig(v, key)
            if r is not None: return r
    elif isinstance(o, list):
        for v in o:
            r = dig(v, key)
            if r is not None: return r
    return None
dseq = dig(d, "dseq")
if dseq is None:
    sys.exit("no dseq in the create response — read created.json")
print("DSEQ_NEW=%s" % shlex.quote(str(dseq)))
PY
)"
echo "created deployment $DSEQ_NEW"

# The manifest submitted with the lease must be the one the deployment was
# created with, so it is taken from the create response when present and rebuilt
# from the SDL only if it is not.
python3 - "$WORK/created.json" "$WORK/sdl.yaml" "$WORK/manifest.txt" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
def dig(o, key):
    if isinstance(o, dict):
        if key in o: return o[key]
        for v in o.values():
            r = dig(v, key)
            if r is not None: return r
    elif isinstance(o, list):
        for v in o:
            r = dig(v, key)
            if r is not None: return r
    return None
m = dig(d, "manifest")
open(sys.argv[3], "w").write(m if isinstance(m, str) else "")
PY

# --- 2. wait for bids --------------------------------------------------------
# Providers bid asynchronously; an empty list a second after creating means
# nobody has answered yet, not that nobody will.
echo -n "waiting for bids"
for _ in $(seq 1 30); do
  curl -sS -m 60 -H "x-api-key: ${AKASH_API_KEY}" "$API/bids/$DSEQ_NEW" -o "$WORK/bids.json" || true
  if python3 -c "
import json,sys
d=json.load(open('$WORK/bids.json'))
b=d.get('data',d)
sys.exit(0 if isinstance(b,list) and b else 1)
" 2>/dev/null; then echo; break; fi
  echo -n "."; sleep 5
done

python3 - "$WORK/bids.json" <<'PY' || { echo "no bids — read bids.json" >&2; exit 1; }
import json, sys
d = json.load(open(sys.argv[1]))
b = d.get("data", d)
if not (isinstance(b, list) and b): sys.exit(1)
print("bids:")
for x in b:
    bid = x.get("bid", x)
    bid_id = bid.get("bid_id", bid.get("id", {}))
    price = bid.get("price", {})
    print("  %-45s %s %s" % (bid_id.get("provider"), price.get("amount"), price.get("denom")))
PY

# Cheapest unless told otherwise. Price is the only thing distinguishing bids
# from here; reputation and location are not in this response.
CHOSEN="$PROVIDER"
if [ -z "$CHOSEN" ]; then
  CHOSEN=$(python3 - "$WORK/bids.json" <<'PY'
import json, sys
b = json.load(open(sys.argv[1]))
b = b.get("data", b)
def price(x):
    bid = x.get("bid", x)
    try: return float(bid.get("price", {}).get("amount", "inf"))
    except (TypeError, ValueError): return float("inf")
best = min(b, key=price)
bid = best.get("bid", best)
print((bid.get("bid_id") or bid.get("id") or {}).get("provider", ""))
PY
)
fi
[ -n "$CHOSEN" ] || { echo "could not choose a provider" >&2; exit 1; }
echo "provider: $CHOSEN"

# The backend must not share a provider with the node — its EARTH_NODE_URL would
# then be a hairpin into the same cluster, which hangs instead of failing. Only
# a warning: this script cannot see where the backend is leased.
echo "check the ads-for-gas backend is NOT leased on $CHOSEN (see akash/README.md)"

if [ "$ASSUME_YES" != 1 ]; then
  printf 'take the lease? [y/N] '
  read -r reply
  case "$reply" in y|Y|yes) ;; *) echo "left deployment $DSEQ_NEW created with no lease; close it or run again with --provider"; exit 1 ;; esac
fi

# --- 3. take the lease -------------------------------------------------------
python3 - "$WORK/created.json" "$WORK/manifest.txt" "$CHOSEN" "$DSEQ_NEW" "$WORK/lease.json" <<'PY'
import json, sys
created, manifest_path, provider, dseq, out = sys.argv[1:6]
d = json.load(open(created))
def dig(o, key):
    if isinstance(o, dict):
        if key in o: return o[key]
        for v in o.values():
            r = dig(v, key)
            if r is not None: return r
    elif isinstance(o, list):
        for v in o:
            r = dig(v, key)
            if r is not None: return r
    return None
manifest = open(manifest_path).read()
# Top level, NOT wrapped in {"data": ...} the way creating a deployment is.
# The two endpoints disagree, and posting the create shape here returns a
# validation error naming both fields as missing.
body = {
    "manifest": manifest,
    "leases": [{
        "owner": dig(d, "owner"),
        "dseq": str(dseq),
        # gseq/oseq are 1 for a single-group deployment, which this is —
        # build-sdl.py asserts the three services and one profile.
        "gseq": 1, "oseq": 1,
        "provider": provider,
    }],
}
if not manifest:
    sys.exit("no manifest — it comes back from creating the deployment and cannot "
             "be rebuilt from the SDL here; close this deployment and create again")
json.dump(body, open(out, "w"))
PY

CODE=$(curl -sS -m 300 -X POST \
  -H "x-api-key: ${AKASH_API_KEY}" -H 'content-type: application/json' \
  --data-binary @"$WORK/lease.json" \
  "$API/leases" -o "$WORK/leased.json" -w '%{http_code}')
case "$CODE" in 2??) ;; *) echo "lease failed (http $CODE)" >&2; show_response "$WORK/leased.json"; exit 1 ;; esac

echo "leased $DSEQ_NEW on $CHOSEN"

# The lock the guard above reads. Commit it: it is how the next create (on any
# clone) knows this genesis already has its validator lease.
if [ "${VALIDATOR_KEY:-0}" = 1 ]; then
  python3 - "$LOCK" "$HERE/akash/genesis.sha256" "$DSEQ_NEW" "$CHOSEN" <<'REC'
import datetime, json, sys
lock, pin_path, dseq, provider = sys.argv[1:5]
json.dump({"genesis_sha256": open(pin_path).read().split()[0], "dseq": dseq, "provider": provider,
           "created": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")},
          open(lock, "w"), indent=2)
open(lock, "a").write("\n")
REC
  echo "recorded akash/validator-lease.lock: commit it"
fi

# --- 4. leave the operator with what changed ---------------------------------
# DSEQ is written back because a stale one silently points deploy.sh at a
# deployment that no longer exists, and PUT against it fails in a way that reads
# like an API problem rather than a wrong number.
cp "$HERE/.env" "$HERE/.env.bak"
python3 - "$HERE/.env" "$DSEQ_NEW" "$FULLNODE" "$DSEQ_VAR" <<'PYEOF'
import re, sys
path, dseq, fullnode = sys.argv[1], sys.argv[2], sys.argv[3] == "1"
explicit = sys.argv[4] if len(sys.argv) > 4 else ""
# A sync node is an ADDITION, not a replacement: the live lease keeps DSEQ, so
# every tool that reads it keeps watching the validator that still holds the
# chain. Only the swap makes the new lease authoritative, and that is a
# deliberate edit, never a side effect of creating a second lease.
var = explicit or ("SYNC_DSEQ" if fullnode else "DSEQ")
s = open(path).read()
s2, n = re.subn(r"^%s=.*$" % var, "%s=%s" % (var, dseq), s, flags=re.M)
open(path, "w").write(s2 if n else s.rstrip("\n") + "\n%s=%s\n" % (var, dseq))
PYEOF
if [ -n "$DSEQ_VAR" ]; then
  echo "$DSEQ_VAR written to .env; DSEQ and SYNC_DSEQ left untouched"
elif [ "$FULLNODE" = 1 ]; then
  echo "SYNC_DSEQ written to .env; DSEQ still points at the live validator"
else
  echo "DSEQ updated in .env (previous kept as .env.bak)"
fi

cat <<EOF

still to do:
  1. point the backend's EARTH_NODE_URL at https://lcd.erth.network and redeploy it
  2. the chain is at height 1 — Cloudflare needs no change, the tunnel token is the same

not urgent, only matters once a second node exists:
  3. EXTERNAL_ADDRESS in akash/deploy.yaml still names the old lease's provider
     port. Read the new one from the lease status and deploy.sh it.
EOF
