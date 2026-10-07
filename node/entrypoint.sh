#!/usr/bin/env bash
#
# The validator's entrypoint: this repository's node image (node/Dockerfile),
# built on the chain's generic image by digest. The chain image ships earthd,
# the release genesis and a minimal entrypoint any operator can run; this one
# replaces it with ours: the consensus and node keys from the environment, the
# keyless guard, refusing a foreign genesis, cosmovisor, and the settings our
# SDL passes (akash/deploy.yaml). Tested by node/entrypoint_test.sh.
#
# Starts an earth node against a genesis it verifies rather than one it invents.
#
# There are three paths and the difference between them is the difference
# between joining a network and creating one:
#
#   resume     $EARTH_HOME/config/genesis.json already exists. Start.
#   join       It does not. Copy the genesis baked into the image, check it
#              against the sha256 published with the release, start. No key is
#              created, no timestamp is rewritten, and every node that boots this
#              image computes the same genesis and the same app hash. This is
#              the default.
#   DEV_INIT=1 It does not, and you asked for a throwaway chain. Generate a
#              validator, stamp genesis_time to now, collect a gentx. Every node
#              doing this makes a DIFFERENT chain, which is exactly what a local
#              devnet wants and exactly what a network cannot tolerate.
#
# The join path is the whole point: two containers from the same image share
# a chain.
#
# State lives entirely under $EARTH_HOME, which is a mounted volume. The genesis
# check is what makes a redeploy resume the existing chain rather than silently
# starting a new one.
set -euo pipefail

# shellcheck source=drop-root.sh
. "$(dirname "$0")/drop-root.sh"

EARTH_HOME="${EARTH_HOME:-/data}"
CHAIN_ID="${CHAIN_ID:-earth-1}"
MONIKER="${MONIKER:-earth-node}"
# The floor below which this node will not relay a transaction. Per-node config,
# not a chain parameter — there is no fee module, so the network's effective
# minimum is whatever most validators set, and changing it is a restart rather
# than a governance vote.
#
# 0uerth means free transactions, which means free spam. 0.005 is ~500uerth on a
# typical transaction — nothing to a user — while filling every block for a day
# would cost a spammer real ERTH.
MIN_GAS_PRICES="${MIN_GAS_PRICES:-0.005uerth}"

# Browser access, which the two RPC surfaces handle very differently.
#
# RPC_CORS_ORIGINS is a comma-separated allowlist for the CometBFT RPC (26657):
#
#   RPC_CORS_ORIGINS=https://app.erth.network,https://wallet.erth.network
#
# This is the one that matters for a CosmJS app. StargateClient talks to the RPC,
# not the LCD, and the RPC ships with cors_allowed_origins = [] — so a browser has
# never been able to reach it cross-origin, on any deployment of this chain. Use
# "*" only if you mean it.
RPC_CORS_ORIGINS="${RPC_CORS_ORIGINS:-}"
#
# API_UNSAFE_CORS opens the LCD (1317) to *any* origin. It is all-or-nothing
# because that is all the SDK offers — server/config has a bool and no allowlist —
# which is why it keeps the "unsafe" in its name. Prefer scoping the RPC above and
# pointing browsers there; reach for this only when something genuinely needs the
# REST surface from a page.
API_UNSAFE_CORS="${API_UNSAFE_CORS:-0}"

# Peering. The node already listens for p2p on 0.0.0.0:26656; these are what
# make it reachable and what give it somewhere to dial.
#
# EXTERNAL_ADDRESS is the address other nodes should use to reach this one,
# host:port. It matters more than it looks: without it CometBFT advertises the
# address it sees on itself, which inside a container is a private address, and
# it hands that to every peer through PEX. The node then appears in the network
# under an address nobody outside can dial. On Akash the provider maps 26656 to
# a port it chooses, so this has to be the provider's hostname and *that* port,
# which is only known once the lease is up.
EXTERNAL_ADDRESS="${EXTERNAL_ADDRESS:-}"
#
# SEEDS are crawlers that hand out peer addresses and then disconnect;
# PERSISTENT_PEERS are nodes to hold a connection to and redial. Both are
# comma-separated `nodeid@host:port`. A node with neither and no peer book has
# no way to find the network — the address book is the only other source, and on
# a fresh volume it is empty.
# Record whether these were SET, not just whether they are non-empty. An
# explicitly empty PERSISTENT_PEERS means "this node is deliberately alone"
# and has to CLEAR config.toml; leaving the value already written there is
# how a node keeps dialling a peer whose lease was closed.
PERSISTENT_PEERS_SET="${PERSISTENT_PEERS+yes}"
SEEDS="${SEEDS:-}"
PERSISTENT_PEERS="${PERSISTENT_PEERS:-}"

# State sync. The node restores state at a recent height from a peer's snapshot
# instead of replaying every block, and starts from there.
#
# This matters more here than on most chains, and for two reasons. Replaying a
# block re-executes its transactions, and every passport registration verifies a
# zkSNARK -- so replay cost grows with adoption, not just with time. And a chain
# that has performed consensus-breaking upgrades cannot be replayed at all by a
# single binary; state sync skips the whole problem, because it never executes a
# block from before the height it lands on.
#
# Needs a trusted height and the hash of that block, from a source you trust --
# your own node, a block explorer, someone you asked. Two rpc_servers are
# required by CometBFT; the same URL twice is accepted and is what a
# single-endpoint network has to do.
STATESYNC_RPC_SERVERS="${STATESYNC_RPC_SERVERS:-}"
STATESYNC_TRUST_HEIGHT="${STATESYNC_TRUST_HEIGHT:-}"
STATESYNC_TRUST_HASH="${STATESYNC_TRUST_HASH:-}"
STATESYNC_TRUST_PERIOD="${STATESYNC_TRUST_PERIOD:-168h0m0s}"

# Where the release genesis and its hash live in the image. Overridable only so
# the three paths below can be exercised without building a container — see
# node/entrypoint_test.sh.
GENESIS_SRC="${GENESIS_SRC:-/etc/earth/genesis.json}"
GENESIS_SHA="${GENESIS_SHA:-${GENESIS_SRC}.sha256}"

# State-sync snapshots. This node produces them so that a NEW node can join by
# downloading state at a height instead of replaying every block from genesis.
#
# Worth more on this chain than on most. Replaying a block re-executes its
# transactions, and every passport registration verifies a zkSNARK
# (x/personhood, ultrahonk.Verify). A chain that mostly moves tokens replays
# quickly; this one re-runs a proof per registration, so sync cost grows with
# adoption rather than with time alone.
#
# The SDK default is 0 — off. A chain launched that way has no node offering
# snapshots, nobody can state-sync, and a snapshot cannot be produced for a
# height already passed. It is cheap now and unavailable later, which is the only
# reason it is on by default here.
#
# 1000 blocks is roughly 80 minutes at 5s. Keeping 5 gives a joining node a
# choice of recent heights without holding much disk.
SNAPSHOT_INTERVAL="${SNAPSHOT_INTERVAL:-1000}"
SNAPSHOT_KEEP_RECENT="${SNAPSHOT_KEEP_RECENT:-5}"

# Devnet-only. See the header: this makes a new chain, not a node on yours.
DEV_INIT="${DEV_INIT:-0}"
# What the devnet validator holds, and how much of it is bonded. Ignored unless
# DEV_INIT=1, because the join path creates no accounts.
VALIDATOR_COINS="${VALIDATOR_COINS:-1000000000000uerth}"
VALIDATOR_BONDED="${VALIDATOR_BONDED:-100000000uerth}"

say() { printf '[entrypoint] %s\n' "$*"; }
die() { printf '[entrypoint] FATAL: %s\n' "$*" >&2; exit 1; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# `sed -i` takes a mandatory suffix on BSD and a forbidden one on GNU, so the
# same invocation cannot work on both. The image is Linux, but this script is
# also run directly by its tests and by anyone debugging on a Mac.
# set_config writes `key = "value"` into a TOML file and checks that it landed.
#
# sed does nothing when its pattern misses, and says nothing about it. For the
# peering settings that silence is the worst outcome available: the node starts,
# logs that it is advertising an address, and is unreachable — which is the
# exact failure those settings exist to prevent. So the write is verified, and a
# miss stops the node instead of producing one that looks healthy from inside.
set_config() {
  local key="$1" value="$2" file="$3"
  sed_inplace "s|^$key = .*|$key = \"$value\"|" "$file"
  grep -q "^$key = \"$value\"$" "$file" || die "could not set $key in $file.
    There is no '$key = ' line to replace, which means this config was written
    by a version of CometBFT that spells it differently. Refusing to start
    rather than run with the setting silently ignored."
}

# Sets `key = value` inside one [section], writing the value verbatim.
#
# set_config cannot do this job twice over. It quotes the value, and CometBFT
# wants trust_height as an integer and enable as a bool -- quoted, the file no
# longer parses. And it replaces every matching line in the file, while `enable`
# is not unique across config.toml: today [statesync] is the only section with a
# bare `enable`, which is luck rather than a guarantee.
set_config_in_section() {
  local section="$1" key="$2" value="$3" file="$4"
  awk -v sec="[$section]" -v k="$key" -v v="$value" '
    /^\[/ { in_sec = ($0 == sec) }
    in_sec && $0 ~ "^" k " = " { print k " = " v; found = 1; next }
    { print }
    END { if (!found) exit 3 }
  ' "$file" > "$file.tmp" || die "no '$key' line in [$section] of $file — this config
    was written by a CometBFT that spells it differently. Refusing to start
    rather than run with the setting silently ignored."
  mv "$file.tmp" "$file"
}

sed_inplace() {
  local expr="$1" file="$2"
  [ -f "$file" ] || die "$file is missing — the node home at $EARTH_HOME is not
    one earthd created. Point EARTH_HOME at a real node home, or delete it and
    let this script initialise one."
  sed "$expr" "$file" > "$file.tmp" && mv "$file.tmp" "$file"
}

# Marker written only after collect-gentxs succeeds. See devnet_is_complete.
DEVINIT_DONE="$EARTH_HOME/config/.devinit-complete"

# Is the genesis on the volume a *finished* devnet genesis?
#
# This exists because the DEV_INIT path below is not atomic and cannot easily be
# made so: it writes config/genesis.json early (earthd init, then cp) and only
# collects the gentx several commands later. Anything that interrupts it in
# between — the pod rescheduled, the container killed — leaves a genesis file
# with an empty gen_txs on the volume. Without this check the next start finds a
# genesis, takes the resume path, and earthd dies with
#
#   error during handshake: error on replay: validator set is empty after
#   InitGenesis, please ensure at least one validator is initialized ...
#
# on every restart forever, because the resume path never rebuilds. That is
# exactly what happened on the first v0.2.0 lease: the node crash-looped, the
# pod reported available but never ready, and the Console API exposes no logs to
# say why. A devnet genesis with no gentx is broken by construction, so detect
# it and rebuild rather than resuming into a chain that cannot produce a block.
devnet_is_complete() {
  [ -f "$DEVINIT_DONE" ] && return 0
  # No marker: either a half-built chain, or one built by an image from before
  # the marker existed. The gentx is what tells those apart.
  [ -f "$EARTH_HOME/config/genesis.json" ] || return 1
  if grep -q '"gen_txs"[[:space:]]*:[[:space:]]*\[[[:space:]]*\]' "$EARTH_HOME/config/genesis.json"; then
    return 1
  fi
  # Healthy chain from an older image. Adopt it and stop re-checking.
  touch "$DEVINIT_DONE" 2>/dev/null || true
  return 0
}

# A DEV_INIT chain's genesis NEVER matches the image's: it carries a stamped
# genesis_time and a gentx the image's copy does not have. So the mismatch check
# below has to know the difference between "this volume holds a throwaway chain
# this node built" and "this volume holds someone else's chain". Without this,
# turning the check on would break every devnet deployment on its next restart.
devnet_chain_on_disk() {
  # DEV_INIT only. It is tempting to also treat the .devinit-complete marker as
  # "this is a devnet, leave it alone" — that was the first version of this, and
  # it was wrong in the one case that matters. Cutting a devnet over to a real
  # genesis means setting DEV_INIT=0 on a volume that still carries the marker,
  # and honouring the marker there silently skipped the reset: the node resumed
  # the old chain while the operator, the SDL and every published artefact said
  # otherwise. It came up on the old chain with the new consensus key, so it had
  # no voting power and the chain lost its only signer.
  #
  # DEV_INIT=0 is an unambiguous instruction. What is already on the volume does
  # not get to override it.
  [ "$DEV_INIT" = "1" ]
}

# The consensus key on the volume as this boot found it, before any branch below
# runs `earthd init` (which writes a fresh random one). REQUIRE_NO_CONSENSUS_KEY
# uses this to tell a key that was already on the disk -- possibly a former
# validator's -- from the throwaway that init minted a moment ago.
PVK_FILE="$EARTH_HOME/config/priv_validator_key.json"
PVK_AT_BOOT=""
[ -f "$PVK_FILE" ] && PVK_AT_BOOT="$(sha256_of "$PVK_FILE")"

if [ "${REQUIRE_NO_CONSENSUS_KEY:-0}" = "1" ] && [ "$DEV_INIT" = "1" ]; then
  die "REQUIRE_NO_CONSENSUS_KEY=1 with DEV_INIT=1. A devnet signs with the key
    it generates, so these contradict."
fi

if [ "$DEV_INIT" = "1" ] && ! devnet_is_complete; then
  # ── devnet ───────────────────────────────────────────────────────────────
  if [ -f "$EARTH_HOME/config/genesis.json" ]; then
    say "DEV_INIT=1 and the genesis on this volume has no gentx — a previous"
    say "attempt died partway through. Rebuilding rather than resuming a chain"
    say "with an empty validator set."
    # Safe: a genesis with no gentx never produced a block, so there is no
    # history here to lose. Config is rebuilt from scratch below.
    rm -rf "$EARTH_HOME/config" "$EARTH_HOME/data"
  fi
  say "DEV_INIT=1 — creating a NEW throwaway chain (not joining one)"
  earthd init "$MONIKER" --chain-id "$CHAIN_ID" --home "$EARTH_HOME" >/dev/null 2>&1
  cp "$GENESIS_SRC" "$EARTH_HOME/config/genesis.json"

  # Stamp genesis_time to now. CometBFT gives block 1 exactly this time while
  # block 2 gets the wall clock, and the emission — plus the protocol-owned
  # liquidity retirement — is prorated against elapsed time, so a stale file
  # pays out the whole gap in a single block. Rewriting it is why this path
  # cannot be used for a real network: it changes the file, so the hash no
  # longer matches and no two nodes agree.
  NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  sed_inplace "s|\"genesis_time\":[[:space:]]*\"[^\"]*\"|\"genesis_time\": \"$NOW\"|" \
    "$EARTH_HOME/config/genesis.json"
  say "genesis_time stamped to $NOW"

  # --keyring-backend test writes the key unencrypted into the volume. That is
  # acceptable here and only here: this chain is disposable by construction. The
  # join path creates no keys at all, and a real validator's consensus key
  # belongs behind PRIV_VALIDATOR_LADDR.
  KEYRING="--keyring-backend test --home $EARTH_HOME"
  if [ -n "${VALIDATOR_MNEMONIC:-}" ]; then
    say "recovering the devnet validator key from VALIDATOR_MNEMONIC"
    printf '%s\n' "$VALIDATOR_MNEMONIC" | earthd keys add validator --recover $KEYRING >/dev/null
  else
    say "generating a devnet validator key — the mnemonic is printed once, below"
    earthd keys add validator $KEYRING --output json | tee /dev/stderr >/dev/null
    say "store that mnemonic: it is the only copy outside this volume"
  fi

  # add-genesis-account updates auth, bank balances and supply together, which
  # is why this is done with earthd rather than by editing the file.
  #
  # --append when the address is already funded, which it is whenever
  # VALIDATOR_MNEMONIC recovers the account the release genesis already carries.
  # Without this the command fails with "account already exists", the entrypoint
  # exits, and the container sits there running with nothing listening — the pod
  # reports available but never ready, and the tunnel serves 502 with no clue
  # why. That is a latent break: this path only runs on a fresh volume, so a
  # deployment that keeps resuming an existing chain never reaches it.
  VAL_ADDR="$(earthd keys show validator -a $KEYRING)"
  if grep -q "$VAL_ADDR" "$EARTH_HOME/config/genesis.json"; then
    say "validator $VAL_ADDR is already funded in the release genesis — appending"
    earthd genesis add-genesis-account validator "$VALIDATOR_COINS" --append $KEYRING
  else
    earthd genesis add-genesis-account validator "$VALIDATOR_COINS" $KEYRING
  fi
  earthd genesis gentx validator "$VALIDATOR_BONDED" --chain-id "$CHAIN_ID" $KEYRING >/dev/null
  # NOT >/dev/null 2>&1: with both streams discarded, a collect that died
  # would take its reason with it and leave a genesis that looked fine until
  # earthd refused to start.
  earthd genesis collect-gentxs --home "$EARTH_HOME" >/dev/null

  # Refuse to hand earthd a genesis that cannot produce a block, rather than
  # letting it discover that during the ABCI handshake.
  if grep -q '"gen_txs"[[:space:]]*:[[:space:]]*\[[[:space:]]*\]' "$EARTH_HOME/config/genesis.json"; then
    die "collect-gentxs produced a genesis with no gentx — refusing to start"
  fi

  # Last thing, and only on success: this is what stops a later restart from
  # resuming a half-built chain.
  touch "$DEVINIT_DONE"
  say "devnet genesis ready: validator bonded $VALIDATOR_BONDED"

elif [ -f "$EARTH_HOME/config/genesis.json" ] \
     && ! devnet_chain_on_disk \
     && [ "$(sha256_of "$EARTH_HOME/config/genesis.json")" != "$(sha256_of "$GENESIS_SRC")" ] \
     && [ "${RESET_ON_GENESIS_MISMATCH:-0}" = "1" ]; then
  # ── reset ────────────────────────────────────────────────────────────────
  #
  # The volume holds a different chain from the one this image ships. On a
  # live chain that is the wrong image, and the branch below stops for it.
  # RESET_ON_GENESIS_MISMATCH=1 is for throwaway devnets only: throw the old
  # chain away and join the image's one. The validator SDL refuses it
  # (build-sdl.py). Keyed to the hash rather than a plain WIPE=1, it fires
  # once, on the first boot after the genesis changes; restarts afterwards
  # see matching hashes and resume.
  say "genesis on this volume does not match the image, and RESET_ON_GENESIS_MISMATCH=1"
  say "  on disk: $(sha256_of "$EARTH_HOME/config/genesis.json")"
  say "  image:   $(sha256_of "$GENESIS_SRC")"
  say "DESTROYING the existing chain state at $EARTH_HOME"
  rm -rf "$EARTH_HOME/config" "$EARTH_HOME/data"
  earthd init "$MONIKER" --chain-id "$CHAIN_ID" --home "$EARTH_HOME" >/dev/null 2>&1
  cp "$GENESIS_SRC" "$EARTH_HOME/config/genesis.json"
  say "installed the image genesis — $(sha256_of "$EARTH_HOME/config/genesis.json")"

elif [ -f "$EARTH_HOME/config/genesis.json" ]; then
  ON_DISK="$(sha256_of "$EARTH_HOME/config/genesis.json")"
  IN_IMAGE="$(sha256_of "$GENESIS_SRC")"
  if ! devnet_chain_on_disk && [ "$ON_DISK" != "$IN_IMAGE" ]; then
    die "the genesis on this volume is not the one in this image
      on disk: $ON_DISK
      image:   $IN_IMAGE
    These are two different chains. On a live validator or node this means
    the wrong image was deployed: stop and investigate, and deploy the image
    whose genesis matches this volume. Do not wipe the volume to make this
    go away; its chain state (and a validator's signing state) is not
    recoverable. RESET_ON_GENESIS_MISMATCH=1 is for throwaway devnets only."
  fi
  say "existing genesis found — resuming chain at $EARTH_HOME"
  say "genesis sha256 $ON_DISK"

else
  # ── join ─────────────────────────────────────────────────────────────────
  say "no genesis at $EARTH_HOME — installing the release genesis"
  [ -f "$GENESIS_SRC" ] || die "$GENESIS_SRC is missing from the image"
  [ -f "$GENESIS_SHA" ] || die "$GENESIS_SHA is missing from the image"

  WANT="$(awk '{print $1}' "$GENESIS_SHA")"
  GOT="$(sha256_of "$GENESIS_SRC")"
  if [ "$WANT" != "$GOT" ]; then
    die "genesis sha256 mismatch — refusing to start
      expected $WANT
      got      $GOT
    The genesis in this image is not the one it was built with. Do not work
    around this; get an image whose genesis matches the published hash."
  fi
  say "genesis sha256 $GOT — matches the release"

  earthd init "$MONIKER" --chain-id "$CHAIN_ID" --home "$EARTH_HOME" >/dev/null 2>&1
  cp "$GENESIS_SRC" "$EARTH_HOME/config/genesis.json"

  # Deliberately absent from this path: no genesis_time rewrite (it would change
  # the file the hash just vouched for), and no key generation (a validator joins
  # with MsgCreateValidator after height 1, or its gentx is already in the file).
  say "ready to join $CHAIN_ID — no keys created, genesis unmodified"
fi

# ---- injected identity ------------------------------------------------------
#
# `earthd init` mints a random consensus key and a random node key. That is
# correct for a node joining an existing network, and wrong for the node whose
# consensus key is named in the genesis it ships: after any volume reset it
# would come up as a validator nobody has heard of, holding none of the voting
# power the gentx assigned, and the chain would have no signer at all.
#
# So both identities can be supplied. Written after init, which is what created
# the files being replaced.
#
#   PRIV_VALIDATOR_KEY_B64   base64 of priv_validator_key.json. The consensus
#                            key the genesis gentx commits to.
#   NODE_KEY_B64             base64 of node_key.json. Fixes the node id, so the
#                            peer address in the gentx memo and in the docs
#                            keeps working across restarts.
#
# Base64 rather than raw JSON because these travel through a YAML env list in the
# SDL, and a raw `{"address":...}` there is a flow mapping to the YAML parser,
# not a string. Encoding sidesteps the quoting entirely.
#
# Both are secrets. PRIV_VALIDATOR_KEY especially: it is the key that
# double-signs. Prefer PRIV_VALIDATOR_LADDR and a remote signer for anything
# with real stake behind it — see akash/REMOTE_SIGNER.md.
#
# REQUIRE_NO_CONSENSUS_KEY=1 says this node must not be able to sign, and this
# is the check that makes that true rather than merely intended.
#
# The failure it exists for, 2026-09-01: a lease that had been the validator was
# redeployed with an SDL carrying no consensus key, on the belief that "no key in
# the SDL" meant "no key". It does not. The block below only ever OVERWRITES
# priv_validator_key.json; it never removes one, so the file from that volume's
# validator days was still there. CometBFT loaded it, the node saw 100% voting
# power, found no peers, and produced 69 blocks of its own chain at heights the
# real validator was also signing. Nothing was slashed only because it happened
# to be isolated -- had it reached a peer, that is equivocation evidence on a
# chain where one key holds all the stake.
#
# So: refuse to start, loudly, rather than delete. Deleting a consensus key on a
# node's say-so is worse than halting one that should not have been started, and
# the operator may be looking at the only copy.
#
# A keyless node still has a priv_validator_key.json. `earthd init` writes a
# random one, and `earthd start` generates one whenever the file is missing
# (privval.LoadOrGenFilePV in the SDK's start). So "no file" cannot be the
# test: it made a keyless node impossible to start on a fresh volume (init's
# key tripped the guard) and impossible to restart (start's key did). The test
# is instead "the only key on this volume is a throwaway this guard recorded":
#
#   - a key init minted during THIS boot (absent, or different, when the boot
#     began) is random and safe; it is recorded in $THROWAWAY_MARK.
#   - with no key at all, a throwaway is minted here and recorded, so the one
#     earthd start would generate never appears unrecorded.
#   - any other key -- one on the volume before this boot that the mark does not
#     name byte for byte -- is refused, exactly as before.
#   - a key whose public half appears in the genesis (a gentx) is refused
#     whatever the mark says: that is a validator key by definition.
THROWAWAY_MARK="$EARTH_HOME/config/.keyless-throwaway-key.sha256"
pvk_pub() { # the base64 pubkey in a priv_validator_key.json
  tr -d ' \n' < "$1" | sed -n 's/.*"pub_key":{[^}]*"value":"\([^"]*\)".*/\1/p'
}
if [ "${REQUIRE_NO_CONSENSUS_KEY:-0}" = "1" ]; then
  if [ -n "${PRIV_VALIDATOR_KEY_B64:-}" ]; then
    die "REQUIRE_NO_CONSENSUS_KEY=1 and PRIV_VALIDATOR_KEY_B64 is set.
      These contradict. One of them is a mistake, and guessing which would
      either strand a validator or start a second one."
  fi
  if [ -f "$PVK_FILE" ]; then
    PVK_NOW="$(sha256_of "$PVK_FILE")"
    if [ -f "$THROWAWAY_MARK" ] && [ "$(cat "$THROWAWAY_MARK")" = "$PVK_NOW" ]; then
      say "consensus key on this volume is the recorded throwaway"
    elif [ "$PVK_NOW" != "$PVK_AT_BOOT" ]; then
      # Not on the disk when this boot began: earthd init just wrote it.
      printf '%s\n' "$PVK_NOW" > "$THROWAWAY_MARK"
      say "recorded the random consensus key earthd init just wrote as a throwaway"
    else
      die "this node is configured to hold NO consensus key, but
      $PVK_FILE already exists on the volume
      and is not the throwaway this guard recorded.

      A key in that file signs whether or not the SDL mentions one. If this
      volume was ever a validator, starting now risks signing at heights another
      node is also signing -- equivocation, and on this chain one key holds all
      the voting power.

      If this node is genuinely meant to be keyless, move the file aside first:
        mv $PVK_FILE \\
           $PVK_FILE.REMOVED
      (the key is also in .env, so this is reversible.) If this node IS meant to
      sign, drop REQUIRE_NO_CONSENSUS_KEY and pass the key instead."
    fi
  else
    tmp_home="$(mktemp -d)"
    earthd init keyless-throwaway --home "$tmp_home" >/dev/null 2>&1 \
      || die "could not mint a throwaway consensus key"
    [ -f "$tmp_home/config/priv_validator_key.json" ] \
      || die "earthd init wrote no priv_validator_key.json to copy"
    mkdir -p "$EARTH_HOME/config"
    cp "$tmp_home/config/priv_validator_key.json" "$PVK_FILE"
    chmod 600 "$PVK_FILE"
    rm -rf "$tmp_home"
    sha256_of "$PVK_FILE" > "$THROWAWAY_MARK"
    say "no consensus key on this volume — minted and recorded a throwaway"
  fi
  PVK_PUB="$(pvk_pub "$PVK_FILE")"
  [ -n "$PVK_PUB" ] || die "cannot read the public key in $PVK_FILE"
  if [ -f "$EARTH_HOME/config/genesis.json" ] \
     && grep -qF "\"$PVK_PUB\"" "$EARTH_HOME/config/genesis.json"; then
    die "the consensus key on this volume ($PVK_PUB) is named in the genesis:
      it is a validator key, not a throwaway. Refusing to start a node that
      must not sign while it holds one."
  fi
  say "verified: the only consensus key here is a recorded throwaway ($PVK_PUB), this node cannot sign as the validator"
fi

# priv_validator_state.json is deliberately NOT injected. It tracks the last
# height signed, and restoring a stale copy is how a validator double-signs.
if [ -n "${PRIV_VALIDATOR_KEY_B64:-}" ]; then
  printf '%s' "$PRIV_VALIDATOR_KEY_B64" | base64 -d > "$EARTH_HOME/config/priv_validator_key.json" \
    || die "PRIV_VALIDATOR_KEY_B64 is not valid base64"
  chmod 600 "$EARTH_HOME/config/priv_validator_key.json"
  # grep, not a JSON parser: the runtime image is debian-slim with earthd and
  # nothing else, and adding python here to validate a three-field file would be
  # 30MB of image for a check that greps do just as well. The realistic failure
  # is a truncated or empty variable, not subtly malformed JSON.
  grep -q '"priv_key"' "$EARTH_HOME/config/priv_validator_key.json" \
    || die "PRIV_VALIDATOR_KEY_B64 does not decode to a priv_validator_key.json —
      refusing to start with a broken consensus key."
  say "consensus key taken from PRIV_VALIDATOR_KEY_B64"
fi
if [ -n "${NODE_KEY_B64:-}" ]; then
  printf '%s' "$NODE_KEY_B64" | base64 -d > "$EARTH_HOME/config/node_key.json" \
    || die "NODE_KEY_B64 is not valid base64"
  chmod 600 "$EARTH_HOME/config/node_key.json"
  grep -q '"priv_key"' "$EARTH_HOME/config/node_key.json" \
    || die "NODE_KEY_B64 does not decode to a node_key.json — refusing to start with
      a broken node identity."
  say "node id fixed from NODE_KEY_B64: $(earthd comet show-node-id --home "$EARTH_HOME" 2>/dev/null || echo '?')"
fi

# Remote signer, if one is configured.
#
# PRIV_VALIDATOR_LADDR turns the node into something that asks for signatures
# rather than something that can produce them: CometBFT listens on this address
# and an external signer (tmkms) dials in, signs, and returns just the
# signature. The consensus key then lives wherever the signer runs — which is
# the point, because this container runs on hardware someone else owns.
#
# Set in config.toml rather than passed as a flag: it is a config field, and
# writing it here means a restart cannot quietly fall back to the local key.
#
# The signer is the client, so it needs no inbound address of its own. That is
# what makes a home machine on a dynamic IP a viable place to keep the key.
#
# NOTE: setting this does not delete priv_validator_key.json. A real migration
# removes it from this host afterwards — leaving it behind means the key you
# just moved is still sitting on the machine you moved it off.
if [ -n "${PRIV_VALIDATOR_LADDR:-}" ]; then
  sed_inplace "s|^priv_validator_laddr = .*|priv_validator_laddr = \"$PRIV_VALIDATOR_LADDR\"|" \
    "$EARTH_HOME/config/config.toml"
  say "remote signer expected at $PRIV_VALIDATOR_LADDR"
fi

# The app mempool must stay the no-op one (max-txs = -1). Private txs are
# unsigned: the SDK's priority and sender-nonce mempools key every tx by its
# signer and sequence and refuse one with no signers, so a node running either
# would drop every shielded transfer, claim, vote, swap and stake (CometBFT's
# own mempool still orders and gossips them). Forced on every start: app.toml
# lives in the volume and a hand edit must not survive a restart.
sed_inplace "s|^max-txs = .*|max-txs = -1|" "$EARTH_HOME/config/app.toml"

# Snapshots. Applied on every start for the same reason as the CORS settings:
# app.toml lives in the volume, so these can change with a restart.
#
# The default pruning profile keeps 362,880 recent states — far more than the
# snapshot interval — so a snapshot is never asked for a height that has already
# been pruned away. Change pruning and check that still holds.
sed_inplace "s|^snapshot-interval = .*|snapshot-interval = $SNAPSHOT_INTERVAL|" \
  "$EARTH_HOME/config/app.toml"
sed_inplace "s|^snapshot-keep-recent = .*|snapshot-keep-recent = $SNAPSHOT_KEEP_RECENT|" \
  "$EARTH_HOME/config/app.toml"
if [ "$SNAPSHOT_INTERVAL" = "0" ]; then
  say "snapshots OFF — no node can state-sync from this one"
else
  say "snapshots every $SNAPSHOT_INTERVAL blocks, keeping $SNAPSHOT_KEEP_RECENT"
fi

# RPC CORS. Applied on every start, not just first boot: config.toml lives in the
# volume, so an origin can be added or removed with a restart instead of a wipe.
if [ -n "$RPC_CORS_ORIGINS" ]; then
  # ["https://a", "https://b"] — a TOML array of quoted strings.
  RPC_CORS_TOML="$(printf '%s' "$RPC_CORS_ORIGINS" | awk -F, '{
    out = ""
    for (i = 1; i <= NF; i++) {
      gsub(/^[ \t]+|[ \t]+$/, "", $i)
      if ($i == "") continue
      out = out (out == "" ? "" : ", ") "\"" $i "\""
    }
    print "[" out "]"
  }')"
  sed_inplace "s|^cors_allowed_origins = .*|cors_allowed_origins = $RPC_CORS_TOML|" \
    "$EARTH_HOME/config/config.toml"
  say "rpc cors_allowed_origins = $RPC_CORS_TOML"
fi

# Peering, applied on every start like the settings above: config.toml is in the
# volume, so a seed can be added or an external address corrected with a restart
# rather than a wipe.
if [ -n "$EXTERNAL_ADDRESS" ]; then
  set_config external_address "$EXTERNAL_ADDRESS" "$EARTH_HOME/config/config.toml"
  say "advertising $EXTERNAL_ADDRESS to peers"
else
  say "EXTERNAL_ADDRESS unset — peers will be handed whatever address this node sees on itself, which in a container is usually unreachable"
fi
if [ -n "$STATESYNC_RPC_SERVERS" ]; then
  [ -n "$STATESYNC_TRUST_HEIGHT" ] && [ -n "$STATESYNC_TRUST_HASH" ] \
    || die "STATESYNC_RPC_SERVERS is set but STATESYNC_TRUST_HEIGHT/STATESYNC_TRUST_HASH are not.
    State sync verifies the snapshot against a block you already trust; without
    that pair it has nothing to check the restored state against."
  cfg="$EARTH_HOME/config/config.toml"
  set_config_in_section statesync enable       "true"                          "$cfg"
  set_config_in_section statesync rpc_servers  "\"$STATESYNC_RPC_SERVERS\""    "$cfg"
  set_config_in_section statesync trust_height "$STATESYNC_TRUST_HEIGHT"       "$cfg"
  set_config_in_section statesync trust_hash   "\"$STATESYNC_TRUST_HASH\""      "$cfg"
  set_config_in_section statesync trust_period "\"$STATESYNC_TRUST_PERIOD\""    "$cfg"
  say "state sync ON from height $STATESYNC_TRUST_HEIGHT via $STATESYNC_RPC_SERVERS"
fi

if [ -n "$SEEDS" ]; then
  set_config seeds "$SEEDS" "$EARTH_HOME/config/config.toml"
  say "seeds = $SEEDS"
fi
if [ -n "$PERSISTENT_PEERS_SET" ]; then
  # Written even when empty. config.toml lives on the volume and persists across
  # deploys, so the only way to un-set a peer is to write the empty value over
  # it. Skipping that on empty leaves a closed lease's address in the config for
  # ever, and CometBFT retries it every few seconds.
  set_config persistent_peers "$PERSISTENT_PEERS" "$EARTH_HOME/config/config.toml"
  if [ -n "$PERSISTENT_PEERS" ]; then
    say "persistent_peers = $PERSISTENT_PEERS"
  else
    say "persistent_peers cleared — this node is deliberately alone"
    # The address book is a separate cache, also on the volume. Without dropping
    # it, PEX keeps dialling the peers it remembers regardless of config.toml.
    rm -f "$EARTH_HOME/config/addrbook.json"
  fi
fi

# Bind to every interface. The defaults listen on loopback, which inside a
# container means nothing outside it can reach the node.
START_ARGS=(
  --home "$EARTH_HOME"
  --moniker "$MONIKER"
  --minimum-gas-prices "$MIN_GAS_PRICES"
  --rpc.laddr tcp://0.0.0.0:26657
  --p2p.laddr tcp://0.0.0.0:26656
  --api.enable
  --api.address tcp://0.0.0.0:1317
)
if [ "$API_UNSAFE_CORS" = "1" ]; then
  say "WARNING: API_UNSAFE_CORS=1 — any origin can read this LCD and broadcast through it"
  START_ARGS+=(--api.enabled-unsafe-cors)
fi

# ---- cosmovisor ------------------------------------------------------------
#
# Off unless USE_COSMOVISOR=true, the same way ENABLED gates the relayer. Anyone
# running this image without the switch gets the old behaviour exactly: earthd,
# directly, no supervisor in the path.
#
# What it buys: at an upgrade height the chain halts on purpose and refuses to
# continue on the old binary. Without cosmovisor a human has to notice and put
# the new binary in place -- on Akash, that means redeploying the SDL with a new
# image digest while the chain sits stopped. With it, cosmovisor reads the plan
# the chain wrote to data/upgrade-info.json, fetches the binary named in the
# governance proposal, verifies its checksum, swaps it in and restarts.
if [ "${USE_COSMOVISOR:-false}" = "true" ]; then
  command -v cosmovisor >/dev/null || die "USE_COSMOVISOR=true but cosmovisor is not in this image"

  # DAEMON_HOME must be on the mounted volume. Cosmovisor keeps downloaded
  # upgrade binaries under $DAEMON_HOME/cosmovisor/, and on Akash everything
  # outside /data is gone on the next container start -- so a DAEMON_HOME on the
  # container filesystem would download the upgrade, restart, and find the
  # binary it just installed missing.
  export DAEMON_HOME="${DAEMON_HOME:-$EARTH_HOME}"
  export DAEMON_NAME=earthd
  export DAEMON_RESTART_AFTER_UPGRADE="${DAEMON_RESTART_AFTER_UPGRADE:-true}"
  export DAEMON_ALLOW_DOWNLOAD_BINARIES="${DAEMON_ALLOW_DOWNLOAD_BINARIES:-true}"

  # Never let this default to false. With downloads enabled, cosmovisor runs
  # whatever the upgrade plan points at; the checksum in the URL is the only
  # thing tying that download to what governance actually approved.
  export DAEMON_DOWNLOAD_MUST_HAVE_CHECKSUM="${DAEMON_DOWNLOAD_MUST_HAVE_CHECKSUM:-true}"

  # Cosmovisor copies data/ before applying an upgrade, which needs as much free
  # space again as the chain currently occupies. Worth keeping: it is the only
  # rollback that exists if an upgrade handler corrupts state. Set
  # UNSAFE_SKIP_BACKUP=true only if the volume genuinely cannot hold two copies.
  export UNSAFE_SKIP_BACKUP="${UNSAFE_SKIP_BACKUP:-false}"

  # Which cosmovisor slot the image binary belongs in depends on where this node
  # starts, and the two answers are opposites.
  #
  #   replaying from genesis  the image must be the LAUNCH binary, in
  #                           cosmovisor/genesis/bin, so cosmovisor walks each
  #                           upgrade in turn and swaps at every plan height.
  #
  #   state syncing           the node lands at a recent height, PAST every
  #                           upgrade, and must run the CURRENT state machine
  #                           from its first block. Starting it on the launch
  #                           binary is a guaranteed app-hash mismatch: a fresh
  #                           volume has no upgrade-info.json, so nothing would
  #                           ever tell cosmovisor to swap.
  #
  # Putting the image in genesis/bin unconditionally -- which this did -- is
  # correct for the first and silently wrong for the second. So the choice
  # follows the mode the operator actually configured.
  gen_bin="$DAEMON_HOME/cosmovisor/genesis/bin"
  mkdir -p "$gen_bin" "$DAEMON_HOME/cosmovisor/upgrades"

  if [ -n "$STATESYNC_RPC_SERVERS" ] && [ ! -L "$DAEMON_HOME/cosmovisor/current" ]; then
    # State sync, on a volume that has not upgraded yet: run the image binary as
    # `current`, not as the genesis binary.
    #
    # The directory is named for the image's version so `cosmovisor run version`
    # and the logs say something true. Its name is not matched against any plan:
    # a later upgrade creates its own directory and repoints `current`, so this
    # one is simply where this node began.
    img_ver="$(earthd version 2>/dev/null || echo image)"
    [ -n "$img_ver" ] || img_ver=image
    sync_dir="$DAEMON_HOME/cosmovisor/upgrades/$img_ver"
    mkdir -p "$sync_dir/bin"
    ln -sf "$(command -v earthd)" "$sync_dir/bin/earthd"
    ln -sfn "$sync_dir" "$DAEMON_HOME/cosmovisor/current"
    # genesis/bin is still populated: cosmovisor reads it on some paths, and an
    # operator who later turns state sync off gets a working node rather than a
    # missing binary.
    ln -sf "$(command -v earthd)" "$gen_bin/earthd"
    say "state sync: running the image binary ($img_ver) as cosmovisor current, not as the genesis binary"
  else
    # A symlink rather than a copy, deliberately: the image is the source of
    # truth for the base binary, so pinning a new image digest also updates what
    # cosmovisor runs before any upgrade has happened.
    ln -sf "$(command -v earthd)" "$gen_bin/earthd"
  fi

  # After an upgrade, cosmovisor's `current` symlink points at the downloaded
  # binary under upgrades/, NOT at the image. From that moment the running chain
  # is whatever governance published rather than whatever digest the SDL pins --
  # which is the intended behaviour, and also the thing to remember when the
  # deployed digest and `earthd version` disagree.
  if [ -L "$DAEMON_HOME/cosmovisor/current" ]; then
    say "cosmovisor: current -> $(readlink "$DAEMON_HOME/cosmovisor/current")"
  fi

  say "starting under cosmovisor (downloads=$DAEMON_ALLOW_DOWNLOAD_BINARIES, checksum required=$DAEMON_DOWNLOAD_MUST_HAVE_CHECKSUM, backup=$([ "$UNSAFE_SKIP_BACKUP" = "true" ] && echo no || echo yes))"
  exec cosmovisor run start "${START_ARGS[@]}" "$@"
fi

exec earthd start "${START_ARGS[@]}" "$@"
