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
TAG="${1:?usage: deploy.sh <tag> [--print]}"
MODE="${2:-}"

[ -f "$HERE/.env" ] || { echo "no .env — it holds the secrets injected into the submitted SDL" >&2; exit 1; }
set -a; . "$HERE/.env"; set +a
: "${AKASH_API_KEY:?set AKASH_API_KEY in .env}"
: "${DSEQ:?set DSEQ in .env — the deployment to update}"

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

DIGEST="$("$HERE/bin/digest.sh" "$TAG")"
python3 "$HERE/bin/build-sdl.py" "$HERE" "$WORK/sdl.yaml" "$DIGEST"

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
