#!/usr/bin/env bash
# Generate fresh launch keys and write their mnemonics straight into the .env
# files, printing only the public addresses:
#   validator operator account -> earth-network-deploy/.env   VALIDATOR_MNEMONIC
#   gas-grant hot wallet       -> earth-network-backend/.env  GAS_WALLET_MNEMONIC
# Each .env is first copied to .env.bak-<timestamp> (mode 600). The mnemonics
# never reach the terminal: back them up from the .env files yourself.
#
#   ./bin/rotate-launch-keys.sh [path/to/earthd]
set -euo pipefail

# Paths default to the main checkouts next to each other under one projects
# directory; override with DEPLOY_ENV, BACKEND_ENV and CHAIN_DIR.
ROOT="${PROJECTS_DIR:-$HOME/Documents/projects}"
DEPLOY_ENV="${DEPLOY_ENV:-$ROOT/earth-network-deploy/.env}"
BACKEND_ENV="${BACKEND_ENV:-$ROOT/earth-network-backend/.env}"
CHAIN="${CHAIN_DIR:-$ROOT/earth-network}"

WORK="$(mktemp -d)"
chmod 700 "$WORK"
trap 'rm -rf "$WORK"' EXIT

EARTHD="${1:-}"
if [ -z "$EARTHD" ]; then
  echo "building earthd from $CHAIN ..." >&2
  (cd "$CHAIN" && go build -o "$WORK/earthd" ./cmd/earthd)
  EARTHD="$WORK/earthd"
fi

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
for f in "$DEPLOY_ENV" "$BACKEND_ENV"; do
  [ -f "$f" ] || { echo "missing $f" >&2; exit 1; }
  cp -p "$f" "$f.bak-$stamp"
  chmod 600 "$f.bak-$stamp"
done

# new_key <env file> <variable> <label>: makes a key, writes its mnemonic to the
# variable in the env file, prints the label and address.
new_key() {
  local env="$1" var="$2" label="$3"
  "$EARTHD" keys add "$label" --keyring-backend test --home "$WORK/home" --output json >"$WORK/$label.json" 2>&1
  python3 - "$WORK/$label.json" "$env" "$var" "$label" <<'PY'
import json, os, re, sys
src, env, var, label = sys.argv[1:]
k = json.loads(next(l for l in open(src).read().splitlines() if l.lstrip().startswith("{")))
m, addr = k["mnemonic"].strip(), k["address"]
assert len(m.split()) == 24 and addr.startswith("earth1"), "unexpected key output"
lines = open(env).read().splitlines()
out, done = [], False
for l in lines:
    if re.match(rf"^{var}=", l):
        out.append(f"{var}='{m}'"); done = True
    else:
        out.append(l)
if not done:
    out.append(f"{var}='{m}'")
tmp = env + ".tmp"
with open(tmp, "w") as f:
    f.write("\n".join(out) + "\n")
os.chmod(tmp, 0o600)
os.replace(tmp, env)
os.remove(src)
print(f"{label}: {addr}")
PY
}

new_key "$DEPLOY_ENV" VALIDATOR_MNEMONIC validator
new_key "$BACKEND_ENV" GAS_WALLET_MNEMONIC gaswallet

echo
echo "Mnemonics written to:"
echo "  $DEPLOY_ENV   (VALIDATOR_MNEMONIC)"
echo "  $BACKEND_ENV  (GAS_WALLET_MNEMONIC)"
echo "Old files kept as .env.bak-$stamp. Back up both new mnemonics offline now."
