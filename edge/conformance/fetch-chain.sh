#!/usr/bin/env bash
#
# Materialise the chain at the commit chain.pin names into .chain/ (go.mod,
# proto/, networks/genesis.json and app/resultcap/ only, from `git archive`, so
# a working tree's uncommitted edits never stand in for the pinned commit), and download the SDK version that go.mod
# requires into the module cache, where the tests read the SDK's protos.
#
# Why not a go.mod require on the chain module: the chain replaces modules with
# paths inside its own tree (third_party/barretenberg-go), which a requiring
# module cannot resolve, and the tests need its .proto files, which are not Go.
#
#   ./fetch-chain.sh                                   fetch from the pinned repo
#   EARTH_CHAIN_SRC=~/src/earth-chain ./fetch-chain.sh  read a local clone that
#                                                      holds the commit (dev, or
#                                                      before it is pushed)
set -euo pipefail
cd "$(dirname "$0")"

repo=$(sed -n 's/^repo=//p' chain.pin)
commit=$(sed -n 's/^commit=//p' chain.pin)
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { echo "chain.pin: commit must be a full 40-hex sha" >&2; exit 1; }

if [ -f .chain/COMMIT ] && [ "$(cat .chain/COMMIT)" = "$commit" ]; then
  echo ".chain is at $commit"
else
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  if [ -n "${EARTH_CHAIN_SRC:-}" ]; then
    src=$EARTH_CHAIN_SRC
    git -C "$src" cat-file -e "$commit^{commit}" 2>/dev/null \
      || { echo "$src does not hold $commit" >&2; exit 1; }
  else
    src=$tmp/git
    git init -q "$src"
    git -C "$src" fetch -q --depth 1 "$repo" "$commit"
  fi
  mkdir "$tmp/out"
  git -C "$src" archive "$commit" go.mod proto networks/genesis.json app/resultcap | tar -x -C "$tmp/out"
  echo "$commit" > "$tmp/out/COMMIT"
  rm -rf .chain
  mv "$tmp/out" .chain
  echo ".chain fetched at $commit"
fi

# The SDK's protos, at the version the chain requires. The chain must not
# replace the SDK: the tests would read the wrong protos.
if grep -Eq '^[[:space:]]*(replace[[:space:]]+)?github\.com/cosmos/cosmos-sdk[[:space:]]*(v[^[:space:]]+[[:space:]]*)?=>' .chain/go.mod; then
  echo "the chain replaces cosmos-sdk; the conformance tests do not model that" >&2; exit 1
fi
sdk=$(sed -nE 's/^[[:space:]]*github\.com\/cosmos\/cosmos-sdk (v[^[:space:]]+).*/\1/p' .chain/go.mod | head -1)
[ -n "$sdk" ] || { echo ".chain/go.mod requires no cosmos-sdk" >&2; exit 1; }
# Outside any module, so this module's go.mod and go.sum are not touched.
dl=$(mktemp -d)
(cd "$dl" && GOFLAGS= go mod download github.com/cosmos/cosmos-sdk@"$sdk")
rmdir "$dl"
echo "cosmos-sdk $sdk in the module cache"
