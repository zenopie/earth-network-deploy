#!/bin/sh
# IBC relayer entrypoint. Off unless ENABLED=true.
#
# A relayer is trustless: it cannot forge packets or move anyone's funds, it only
# submits proofs that both chains verify for themselves. The key it holds pays
# gas and nothing else, which is why this can share a deployment with the node
# without the concerns that surround the consensus key. The worst a broken or
# absent relayer does is delay packets until they time out and refund.
#
# What it CANNOT do is run without funds, on EITHER chain. Delivering a packet
# is a transaction wherever it lands, paid in that chain's token, and earth is
# not free — the node sets minimum-gas-prices. A relayer whose balance runs dry
# on either side stops relaying silently.
set -eu

# shellcheck source=drop-root.sh
. "$(dirname "$0")/drop-root.sh"

say() { printf '[relayer] %s\n' "$*"; }

if [ "${ENABLED:-false}" != "true" ]; then
  say "ENABLED is not 'true' — relayer is off"
  say "set ENABLED=true plus COUNTERPARTY_* and RELAYER_MNEMONIC to turn it on"
  # Idle rather than exit. Exiting would put the service in a crash-loop that
  # looks like a failure in the deployment's status, when being off is intended.
  while true; do sleep 3600; done
fi

: "${COUNTERPARTY_CHAIN_ID:?set COUNTERPARTY_CHAIN_ID when ENABLED=true}"
: "${COUNTERPARTY_RPC:?set COUNTERPARTY_RPC when ENABLED=true}"
: "${RELAYER_MNEMONIC:?set RELAYER_MNEMONIC when ENABLED=true}"

CHAIN_ID="${CHAIN_ID:-earth-1}"
EARTH_RPC="${EARTH_RPC:-http://node:26657}"
# Must be >= the node's minimum-gas-prices (MIN_GAS_PRICES in the SDL, 0.005uerth).
# Defaulted a little above it so a small bump on the node side does not silently
# stop the relayer.
EARTH_GAS_PRICES="${EARTH_GAS_PRICES:-0.006uerth}"
COUNTERPARTY_PREFIX="${COUNTERPARTY_PREFIX:-cosmos}"
COUNTERPARTY_GAS_PRICES="${COUNTERPARTY_GAS_PRICES:-0.025uatom}"
# 118 is the Cosmos default and right for most chains. Ethereum-style chains
# derive keys down coin type 60 and sign with ethsecp256k1 rather than
# secp256k1 — Injective is one. Get either of these wrong and rly derives an
# address that is not the one you funded, then fails with "account not found"
# or an unparseable pubkey, neither of which names the real cause.
#
#   Injective:  COUNTERPARTY_COIN_TYPE=60  COUNTERPARTY_EXTRA_CODECS='"ethermint"'
COUNTERPARTY_COIN_TYPE="${COUNTERPARTY_COIN_TYPE:-118}"
COUNTERPARTY_EXTRA_CODECS="${COUNTERPARTY_EXTRA_CODECS:-}"
PATH_NAME="${PATH_NAME:-earth-hub}"
RLY_HOME="${RLY_HOME:-/data/relayer}"

mkdir -p "$RLY_HOME"

if [ ! -f "$RLY_HOME/config/config.yaml" ]; then
  say "initialising relayer config at $RLY_HOME"
  rly config init --home "$RLY_HOME"

  # coin-type 118 on both sides: Earth uses the Cosmos default (app/app.go).
  # Getting it wrong yields addresses that look correct and hold nothing.
  #
  # EARTH_GAS_PRICES must be at or above the node's minimum-gas-prices, which
  # the SDL sets to 0.005uerth. This said "0uerth" until it was noticed: earth
  # was zero-fee once, the minimum was raised to stop free spam, and the relayer
  # config never followed. Every packet it tried to deliver would have been
  # rejected with "insufficient fees" — while looking, from the outside, like a
  # channel or counterparty problem rather than a fee one.
  cat > /tmp/earth.json <<EOF
{"type":"cosmos","value":{
  "key":"default","chain-id":"$CHAIN_ID","rpc-addr":"$EARTH_RPC",
  "account-prefix":"earth","keyring-backend":"test",
  "gas-adjustment":1.5,"gas-prices":"$EARTH_GAS_PRICES","min-gas-amount":0,
  "debug":false,"timeout":"20s","output-format":"json",
  "sign-mode":"direct","extra-codecs":[],"coin-type":118,
  "broadcast-mode":"batch"}}
EOF
  cat > /tmp/counterparty.json <<EOF
{"type":"cosmos","value":{
  "key":"default","chain-id":"$COUNTERPARTY_CHAIN_ID","rpc-addr":"$COUNTERPARTY_RPC",
  "account-prefix":"$COUNTERPARTY_PREFIX","keyring-backend":"test",
  "gas-adjustment":1.5,"gas-prices":"$COUNTERPARTY_GAS_PRICES","min-gas-amount":0,
  "debug":false,"timeout":"20s","output-format":"json",
  "sign-mode":"direct","extra-codecs":[$COUNTERPARTY_EXTRA_CODECS],
  "coin-type":$COUNTERPARTY_COIN_TYPE,
  "broadcast-mode":"batch"}}
EOF
  rly chains add --file /tmp/earth.json "$CHAIN_ID" --home "$RLY_HOME"
  rly chains add --file /tmp/counterparty.json "$COUNTERPARTY_CHAIN_ID" --home "$RLY_HOME"

  # One mnemonic, both chains. Same key material — but NOT necessarily the same
  # address: a counterparty on coin type 60 derives a different key from the
  # same words than earth does on 118. Check the addresses printed below against
  # the ones you funded.
  rly keys restore "$CHAIN_ID" default "$RELAYER_MNEMONIC" --home "$RLY_HOME"
  rly keys restore "$COUNTERPARTY_CHAIN_ID" default "$RELAYER_MNEMONIC" --home "$RLY_HOME"

  rly paths new "$CHAIN_ID" "$COUNTERPARTY_CHAIN_ID" "$PATH_NAME" --home "$RLY_HOME"

  # Pin an existing path, if one has already been linked.
  #
  # This config directory does not survive a redeploy — the relayer has
  # ephemeral storage — so it is rebuilt on every boot, and `rly paths new`
  # creates a path that knows nothing about the client and connection a previous
  # link established. `rly start` then has nothing to relay: packets pile up on
  # a perfectly healthy open channel while the relayer sits there idle and says
  # nothing. That is exactly what happened after LINK_ON_START was turned off —
  # the first transfer relayed because the link had just written the ids, and
  # the next one stuck at sequence 2.
  #
  # A path in rly is a client plus a connection; the channels follow from the
  # connection, so pinning these is enough to relay every channel on it.
  if [ -n "${SRC_CLIENT_ID:-}" ] && [ -n "${SRC_CONNECTION_ID:-}" ] \
     && [ -n "${DST_CLIENT_ID:-}" ] && [ -n "${DST_CONNECTION_ID:-}" ]; then
    rly paths update "$PATH_NAME" \
      --src-client-id "$SRC_CLIENT_ID" --src-connection-id "$SRC_CONNECTION_ID" \
      --dst-client-id "$DST_CLIENT_ID" --dst-connection-id "$DST_CONNECTION_ID" \
      --home "$RLY_HOME"
    say "path pinned to $SRC_CONNECTION_ID <-> $DST_CONNECTION_ID"
  else
    say "config written — the path has no connection yet; link once, then set"
    say "SRC_/DST_CLIENT_ID and SRC_/DST_CONNECTION_ID so restarts keep relaying"
  fi
fi

# Creating the client/connection/channel is a one-off that spends gas on both
# chains, so it is opt-in rather than something a restart re-attempts.
if [ "${LINK_ON_START:-false}" = "true" ]; then
  say "linking $PATH_NAME (creates client, connection and channel)"

  # --override skips the scan for an existing client to reuse, and on some
  # counterparties that scan is fatal. rly decodes every client the chain hosts,
  # and a chain hosting 08-wasm light clients (Osmosis testnet does — that is
  # how Celestia and Ethereum are bridged) makes it fail before a single
  # transaction is submitted:
  #
  #   no concrete type registered for type URL
  #   /ibc.lightclients.wasm.v1.ClientState against interface *exported.ClientState
  #
  # rly v2.6.0 — the newest release — does not register that type. The cost of
  # skipping the scan is a fresh client rather than a reused one, which is a few
  # thousand gas once. LINK_REUSE_CLIENT=true restores the old behaviour on a
  # counterparty where reuse matters and the scan works.
  # --client-tp-percentage 66, not rly's default of 85.
  #
  # A light client's trusting period must expire BEFORE the counterparty's
  # unbonding period, so that validators who signed a fraudulent header are
  # still bonded — and so still slashable — when the evidence is submitted. The
  # gap between the two is the whole window for detecting misbehaviour and
  # getting proof on chain.
  #
  # Two thirds is the long-standing IBC convention and Hermes' default. rly ships
  # 85%, which leaves a third of the margin: against a 5-day unbonding that is
  # 18 hours to catch a fork instead of 40. It buys nothing but slightly fewer
  # client updates.
  TP_PCT="${CLIENT_TP_PERCENTAGE:-66}"
  if [ "${LINK_REUSE_CLIENT:-false}" = "true" ]; then
    rly transact link "$PATH_NAME" --client-tp-percentage "$TP_PCT" --home "$RLY_HOME"
  else
    rly transact link "$PATH_NAME" --override --client-tp-percentage "$TP_PCT" --home "$RLY_HOME"
  fi
fi

say "relaying $PATH_NAME"
exec rly start "$PATH_NAME" --home "$RLY_HOME"
