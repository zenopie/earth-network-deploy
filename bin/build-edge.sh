#!/usr/bin/env bash
#
# Build earth-edge's image (edge/Dockerfile) for linux/amd64, push it to ghcr,
# and print the digest akash/deploy.yaml's `edge` service must pin.
#
#   bin/build-edge.sh            build, test (in the Dockerfile), push, print
#   bin/build-edge.sh --pin      ... and write the digest into akash/deploy.yaml
#   bin/build-edge.sh --check    build and test only; push nothing
#   bin/build-edge.sh --pin-built <rev>
#                                build nothing: pin the image CI's "images"
#                                workflow pushed for commit <rev> (its tag),
#                                after the same anonymous-pull check
#
# Needs docker with buildx, and for a push `docker login ghcr.io` with a token
# that has write:packages. The package must be public (the Akash provider pulls
# it without credentials); the script checks that by resolving the pushed tag
# anonymously, and refuses a digest that does not match what it pushed.
#
# The image is built from committed code only: edge/ must be clean, and the tag
# is the commit, so a digest always names a commit of this repo.
set -euo pipefail
cd "$(dirname "$0")/.."

REPO="${EDGE_IMAGE_REPO:-zenopie/earth-network-edge}"
SDL=akash/deploy.yaml
pin=0 check=0 built=""
while [ $# -gt 0 ]; do
  case "$1" in
    --pin) pin=1; shift ;;
    --check) check=1; shift ;;
    --pin-built) built="${2:?--pin-built needs the 12-hex commit tag CI pushed}"; pin=1; shift 2 ;;
    *) echo "usage: build-edge.sh [--pin | --check | --pin-built <rev>]" >&2; exit 2 ;;
  esac
done
[ $((pin + check)) -le 1 ] || { echo "--pin and --check exclude each other" >&2; exit 2; }

if [ -n "$built" ]; then
  [[ "$built" =~ ^[0-9a-f]{12}$ ]] || { echo "--pin-built takes the 12-hex commit tag" >&2; exit 2; }
  [ "$check" = 0 ] || { echo "--pin-built and --check exclude each other" >&2; exit 2; }
  git cat-file -e "${built}^{commit}" 2>/dev/null || { echo "${built} is not a commit here" >&2; exit 1; }
  [ -z "$(git diff --name-only "$built" HEAD -- edge)" ] \
    || { echo "edge/ changed since ${built}: that image is not this checkout's filter" >&2; exit 1; }
  [ -z "$(git status --porcelain -- edge)" ] || { echo "edge/ has uncommitted changes" >&2; exit 1; }
  remote=$(IMAGE_REPO="$REPO" bin/digest.sh "$built") || {
    echo "${REPO}:${built} cannot be read anonymously: not pushed, or the package is not public" >&2; exit 1; }
  digest="${remote##*@}"
else
if [ -n "$(git status --porcelain -- edge)" ]; then
  echo "edge/ has uncommitted changes: commit them first, so the digest names a commit" >&2
  exit 1
fi
rev=$(git rev-parse --short=12 HEAD)
ref="ghcr.io/${REPO}:${rev}"

if [ "$check" = 1 ]; then
  docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
    --output type=cacheonly edge
  echo "built and tested ${rev}; nothing pushed"
  exit 0
fi

meta=$(mktemp); trap 'rm -f "$meta"' EXIT
# --provenance/--sbom off: one image manifest, so the digest is that manifest's
# and the same one the registry returns for the tag.
docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
  --metadata-file "$meta" --tag "$ref" --push edge
digest=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["containerimage.digest"])' "$meta")
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "no digest in buildx metadata" >&2; exit 1; }

# What an anonymous puller (the provider) gets for the tag must be what we pushed.
remote=$(IMAGE_REPO="$REPO" bin/digest.sh "$rev") || {
  echo "pushed, but ${REPO} cannot be read anonymously: make the ghcr package public" >&2; exit 1; }
[ "$remote" = "ghcr.io/${REPO}@${digest}" ] || {
  echo "registry has ${remote} for ${rev}, buildx pushed ${digest}" >&2; exit 1; }

fi
pinned="ghcr.io/${REPO}@${digest}"
echo "$pinned"

if [ "$pin" = 1 ]; then
  python3 - "$SDL" "$REPO" "$pinned" <<'PY'
import re, sys
path, repo, pinned = sys.argv[1:]
s = open(path).read()
pat = r'(?m)^(\s*image:\s*)ghcr\.io/' + re.escape(repo) + r'@sha256:[0-9a-f]{64}[ \t]*$'
s, n = re.subn(pat, lambda m: m.group(1) + pinned, s)
assert n == 1, "expected exactly one %s image line in %s, found %d" % (repo, path, n)
open(path, "w").write(s + ("" if s.endswith("\n") else "\n"))
print("pinned edge in %s; commit it" % path)
PY
fi
