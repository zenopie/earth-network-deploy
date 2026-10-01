#!/usr/bin/env bash
#
# Deploy a released tag to the Akash lease, in place.
#
#   bin/deploy.sh v0.4.5           update the running deployment
#   bin/deploy.sh v0.4.5 --print   build the SDL and show it, submit nothing
#
# In place means the volumes survive, so the chain keeps its height and history.
# Only image and env changes can go this way: endpoint kinds and resources are
# part of what the provider bid on, and changing those needs a close-and-recreate
# — which destroys the chain's state. See akash/README.md.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TAG="${1:?usage: deploy.sh <tag> [--print] [--sdl <path>] [--dseq <n>] [--fullnode] [--validator-key] [--node-key] [--tunnel] [--tunnel-var NAME] [--no-statesync]}"
shift

# There are two leases during the sync migration, and PUTting the wrong SDL at
# the wrong one is how this goes badly: the live validator's SDL carries the
# consensus key, the sync node's must not until the swap. Both are named
# explicitly rather than defaulted, so a mistake has to be typed rather than
# inherited from .env.
MODE=""; SDL_FILE="akash/deploy.yaml"; TARGET_DSEQ=""; BUILD_ARGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --print)          MODE=--print; shift ;;
    --sdl)            SDL_FILE="${2:?--sdl needs a path}"; shift 2 ;;
    --dseq)           TARGET_DSEQ="${2:?--dseq needs a number}"; shift 2 ;;
    --fullnode)       BUILD_ARGS+=(--fullnode); shift ;;
    --validator-key)  BUILD_ARGS+=(--validator-key); shift ;;
    --no-statesync)   BUILD_ARGS+=(--no-statesync); shift ;;
    --tunnel)         BUILD_ARGS+=(--tunnel); shift ;;
    --node-key)       BUILD_ARGS+=(--node-key); shift ;;
    --tunnel-var)     BUILD_ARGS+=(--tunnel-var "${2:?--tunnel-var needs a name}"); shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -f "$HERE/.env" ] || { echo "no .env — it holds the secrets injected into the submitted SDL" >&2; exit 1; }
set -a; . "$HERE/.env"; set +a
: "${AKASH_API_KEY:?set AKASH_API_KEY in .env}"
DSEQ="${TARGET_DSEQ:-${DSEQ:-}}"
: "${DSEQ:?set DSEQ in .env, or pass --dseq — the deployment to update}"
echo "target: deployment $DSEQ  <-  $SDL_FILE"

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

DIGEST="$("$HERE/bin/digest.sh" "$TAG")"
python3 "$HERE/bin/build-sdl.py" "$HERE" "$WORK/sdl.yaml" "$DIGEST" \
  --sdl "$SDL_FILE" ${BUILD_ARGS[@]+"${BUILD_ARGS[@]}"}

if [ "$MODE" = "--print" ]; then
  # Secrets are in it, so this goes to stdout for a human, never to a file.
  cat "$WORK/sdl.yaml"; exit 0
fi

python3 -c "
import json,sys
json.dump({'data':{'sdl':open(sys.argv[1]).read()}}, open(sys.argv[2],'w'))
" "$WORK/sdl.yaml" "$WORK/body.json"

CODE=$(curl -sS -m 180 -X PUT \
  -H "x-api-key: ${AKASH_API_KEY}" -H 'content-type: application/json' \
  --data-binary @"$WORK/body.json" \
  "https://console-api.akash.network/v1/deployments/${DSEQ}" \
  -o "$WORK/resp.json" -w '%{http_code}')

if [ "$CODE" != "200" ]; then
  echo "deploy failed (http $CODE)" >&2; head -c 600 "$WORK/resp.json" >&2; echo >&2; exit 1
fi
echo "deployed $TAG to $DSEQ"
