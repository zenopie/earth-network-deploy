#!/usr/bin/env bash
#
# Build the validator's node image (node/Dockerfile) on the chain image that
# node/base.pin names, for linux/amd64, push it to ghcr, and print the digest
# akash/deploy.yaml's `node` and `relayer` services must pin.
#
#   bin/build-node.sh --base <chain tag>   resolve the chain release's image
#                                          digest and write node/base.pin;
#                                          builds nothing (commit it, then build)
#   bin/build-node.sh                      build, push, print
#   bin/build-node.sh --pin                ... and write the digest into
#                                          akash/deploy.yaml (node and relayer)
#   bin/build-node.sh --check              build only; push nothing
#
# Needs docker with buildx, and for a push `docker login ghcr.io` with a token
# that has write:packages. The package must be public (the Akash provider pulls
# it without credentials); the script checks that by resolving the pushed tag
# anonymously, and refuses a digest that does not match what it pushed.
#
# The build passes akash/genesis.sha256 too: node/Dockerfile refuses a base
# whose baked genesis differs, and labels the image with the base and the
# genesis (bin/check-node-image.py, run here after the push and by deploy.sh
# and create.sh, reads them back from the registry).
#
# Built from committed files only: node/ must be clean, and the tag is the
# commit, so a digest always names a commit of this repo and, through
# node/base.pin, one chain image. The pinned SDL line carries the base as a
# comment; bin/build-sdl.py refuses a node image whose base is not
# node/base.pin, or not the chain release being deployed.
set -euo pipefail
cd "$(dirname "$0")/.."

REPO="${NODE_IMAGE_REPO:-zenopie/earth-network-node}"
CHAIN_REPO="zenopie/earth-network-chain"
SDL=akash/deploy.yaml
PIN=node/base.pin
pin=0 check=0 base_tag=""
while [ $# -gt 0 ]; do
  case "$1" in
    --pin) pin=1; shift ;;
    --check) check=1; shift ;;
    --base) base_tag="${2:?--base needs the chain release tag}"; shift 2 ;;
    *) echo "usage: build-node.sh [--base <chain tag> | --pin | --check]" >&2; exit 2 ;;
  esac
done
[ $((pin + check)) -le 1 ] || { echo "--pin and --check exclude each other" >&2; exit 2; }

if [ -n "$base_tag" ]; then
  [ $((pin + check)) = 0 ] || { echo "--base writes node/base.pin only; build after committing it" >&2; exit 2; }
  base=$(IMAGE_REPO="$CHAIN_REPO" bin/digest.sh "$base_tag")
  printf '%s\n' "$base" > "$PIN"
  echo "node/base.pin = $base (chain $base_tag); commit it, then bin/build-node.sh --pin"
  exit 0
fi

base=$(tr -d '[:space:]' < "$PIN")
[[ "$base" =~ ^ghcr\.io/${CHAIN_REPO}@sha256:[0-9a-f]{64}$ ]] \
  || { echo "$PIN is not ghcr.io/${CHAIN_REPO}@sha256:<64 hex>: $base" >&2; exit 1; }
[ "${base##*:}" != "$(printf '0%.0s' {1..64})" ] \
  || { echo "$PIN is the placeholder: bin/build-node.sh --base <chain tag> first" >&2; exit 1; }

# The base's baked genesis must be the pinned one: node/Dockerfile fails the
# build otherwise, and labels the image with it.
gsha=$(awk 'NR==1{print $1}' akash/genesis.sha256)
[[ "$gsha" =~ ^[0-9a-f]{64}$ ]] || { echo "akash/genesis.sha256 has no sha256" >&2; exit 1; }

if [ -n "$(git status --porcelain -- node akash/genesis.sha256)" ]; then
  echo "node/ or akash/genesis.sha256 has uncommitted changes: commit them first, so the digest names a commit" >&2
  exit 1
fi
node/entrypoint_test.sh >/dev/null || { echo "node/entrypoint_test.sh fails: not building" >&2; exit 1; }
rev=$(git rev-parse --short=12 HEAD)
ref="ghcr.io/${REPO}:${rev}"

if [ "$check" = 1 ]; then
  docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
    --build-arg "CHAIN_IMAGE=$base" --build-arg "GENESIS_SHA256=$gsha" --output type=cacheonly node
  echo "built ${rev} on ${base}; nothing pushed"
  exit 0
fi

meta=$(mktemp); trap 'rm -f "$meta"' EXIT
# --provenance/--sbom off: one image manifest, so the digest is that manifest's
# and the same one the registry returns for the tag.
docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
  --build-arg "CHAIN_IMAGE=$base" --build-arg "GENESIS_SHA256=$gsha" \
  --metadata-file "$meta" --tag "$ref" --push node
digest=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["containerimage.digest"])' "$meta")
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "no digest in buildx metadata" >&2; exit 1; }

remote=$(IMAGE_REPO="$REPO" bin/digest.sh "$rev") || {
  echo "pushed, but ${REPO} cannot be read anonymously: make the ghcr package public" >&2; exit 1; }
[ "$remote" = "ghcr.io/${REPO}@${digest}" ] || {
  echo "registry has ${remote} for ${rev}, buildx pushed ${digest}" >&2; exit 1; }

pinned="ghcr.io/${REPO}@${digest}"
# Read the labels back from the registry, as a deploy will.
NODE_IMAGE_REPO="$REPO" python3 bin/check-node-image.py "$pinned" "$base" "$gsha" \
  || { echo "the pushed image's labels do not match what was built" >&2; exit 1; }
echo "$pinned  (FROM $base)"

if [ "$pin" = 1 ]; then
  python3 - "$SDL" "$REPO" "$pinned" "$base" <<'PY'
import re, sys
path, repo, pinned, base = sys.argv[1:]
s = open(path).read()
pat = (r'(?m)^(\s*image:\s*)ghcr\.io/' + re.escape(repo) +
       r'@sha256:[0-9a-f]{64}[ \t]*(#[^\n]*)?$')
s, n = re.subn(pat, lambda m: m.group(1) + pinned + '  # FROM ' + base, s)
assert n == 2, "expected the node and relayer %s image lines in %s, found %d" % (repo, path, n)
open(path, "w").write(s + ("" if s.endswith("\n") else "\n"))
print("pinned node and relayer in %s; commit it" % path)
PY
fi
