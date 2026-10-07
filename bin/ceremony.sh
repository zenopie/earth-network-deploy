#!/usr/bin/env bash
#
# Run the chain repo's launch ceremony with earth-1's launch identities.
#
#   bin/ceremony.sh <chain checkout> --genesis-time <RFC3339, UTC> \
#       --memo-peer <node id>@<host>:26656 --moniker <name>
#
# The chain repo holds no launch identities: its scripts/ceremony.sh takes
# them as --launch (launch/launch.json here: the operator, its consensus
# pubkey, the placeholder accounts to remove, the consensus keys never to
# reuse) and the operator's mnemonic from --env-file (this repo's .env; only
# the VALIDATOR_MNEMONIC line is read, and never printed). Everything else is
# passed through. See RELAUNCH.md section 2.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHAIN="${1:?usage: ceremony.sh <chain checkout> --genesis-time <RFC3339> --memo-peer ID@HOST:PORT --moniker NAME}"
shift
[ -x "$CHAIN/scripts/ceremony.sh" ] || { echo "$CHAIN has no scripts/ceremony.sh" >&2; exit 1; }
[ -f "$HERE/.env" ] || { echo "no .env here: it holds VALIDATOR_MNEMONIC" >&2; exit 1; }
for a in "$@"; do
  case "$a" in
    --launch|--launch=*|--env-file|--env-file=*) echo "$a is set by this wrapper" >&2; exit 2 ;;
  esac
done
exec "$CHAIN/scripts/ceremony.sh" --launch "$HERE/launch/launch.json" --env-file "$HERE/.env" "$@"
