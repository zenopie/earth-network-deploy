#!/usr/bin/env bash
#
# Exercises entrypoint.sh's three paths without building a container.
#
# This script decides whether a node joins your network or forks off its own, and
# the failure is silent: a node that invents its own genesis starts, produces
# blocks, and looks healthy right up until you notice it shares a chain with
# nobody. That is worth a test even though it is shell.
#
# earthd is stubbed rather than real. What is under test is the branching, the
# hash check and the flags — not whether the SDK can init a node, which it can.
#
#   node/entrypoint_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRYPOINT="$HERE/entrypoint.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# A stand-in for the release genesis the chain image carries at
# /etc/earth/genesis.json: what the entrypoint reads of it is its bytes (the
# hashes), genesis_time and the gentx's consensus pubkey.
RELEASE_GENESIS="$WORK/release-genesis.json"
cat > "$RELEASE_GENESIS" <<'JSON'
{
  "app_state": {
    "genutil": {
      "gen_txs": [
        {
          "body": {
            "messages": [
              {
                "@type": "/cosmos.staking.v1beta1.MsgCreateValidator",
                "pubkey": {
                  "@type": "/cosmos.crypto.ed25519.PubKey",
                  "key": "Z2VuZXNpcy12YWxpZGF0b3Ita2V5LWZvci10aGUtdGVzdA=="
                }
              }
            ]
          }
        }
      ]
    }
  },
  "chain_id": "earth-1",
  "genesis_time": "2026-10-02T12:00:00Z"
}
JSON

pass=0; fail=0
ok()  { printf '  \033[32mok\033[0m   %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n     %s\n' "$1" "${2:-}"; fail=$((fail+1)); }

# ── a stub earthd that records what it was asked to do ─────────────────────
mkdir -p "$WORK/bin"
cat > "$WORK/bin/earthd" <<'STUB'
#!/usr/bin/env bash
echo "earthd $*" >> "$EARTHD_LOG"
case "$1" in
  init)
    home=""; for ((i=1;i<=$#;i++)); do [ "${!i}" = "--home" ] && j=$((i+1)) && home="${!j}"; done
    mkdir -p "$home/config"
    # Enough of a real config.toml for the settings the entrypoint writes,
    # persistent_peers_max_dial_period included: it shares a prefix with
    # persistent_peers, and a pattern loose enough to hit both would leave
    # CometBFT unable to parse its own config.
    printf 'priv_validator_laddr = ""\ncors_allowed_origins = []\nexternal_address = ""\nseeds = ""\npersistent_peers = ""\npersistent_peers_max_dial_period = "0s"\n' > "$home/config/config.toml"
    # [statesync], plus a decoy `enable` in another section. `enable` is not a
    # unique key in a real config.toml either, and a file-wide substitution
    # would silently switch on whatever else owns one.
    printf '\n[tx_index]\nenable = false\n\n[statesync]\nenable = false\nrpc_servers = ""\ntrust_height = 0\ntrust_hash = ""\ntrust_period = "168h0m0s"\n' >> "$home/config/config.toml"
    printf 'snapshot-interval = 0\nsnapshot-keep-recent = 2\n' > "$home/config/app.toml"
    printf '{"stock":true}\n' > "$home/config/genesis.json"
    # Like the real init (privval.LoadOrGenFilePV): a random consensus key,
    # unless one is already there, which init keeps.
    r="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
    [ -f "$home/config/priv_validator_key.json" ] || printf '{\n  "address": "AB",\n  "pub_key": {\n    "type": "tendermint/PubKeyEd25519",\n    "value": "%s"\n  },\n  "priv_key": {\n    "type": "tendermint/PrivKeyEd25519",\n    "value": "x"\n  }\n}\n' "$r" > "$home/config/priv_validator_key.json"
    ;;
  start)
    echo "STARTED $*" >> "$EARTHD_LOG"
    # Like the real start (privval.LoadOrGenFilePV): a missing consensus key is
    # generated, not an error.
    home=""; for ((i=1;i<=$#;i++)); do [ "${!i}" = "--home" ] && j=$((i+1)) && home="${!j}"; done
    if [ -n "$home" ] && [ ! -f "$home/config/priv_validator_key.json" ]; then
      printf '{"address":"GEN","pub_key":{"type":"tendermint/PubKeyEd25519","value":"started"},"priv_key":{"value":"x"}}' > "$home/config/priv_validator_key.json"
    fi
    ;;
  version) echo "v9.9.9" ;;
esac
exit 0
STUB
chmod +x "$WORK/bin/earthd"
export PATH="$WORK/bin:$PATH"

# ── a genesis and a matching hash, standing in for the image's ─────────────
GEN="$WORK/genesis.json"
cp "$RELEASE_GENESIS" "$GEN"
if command -v sha256sum >/dev/null 2>&1; then sha256sum "$GEN" | awk '{print $1}' > "$GEN.sha256"
else shasum -a 256 "$GEN" | awk '{print $1}' > "$GEN.sha256"; fi

run() { # run <home> [env...] -> writes $LOG, returns entrypoint's exit code
  local home="$1"; shift
  export EARTHD_LOG="$WORK/earthd.log"; : > "$EARTHD_LOG"
  LOG="$WORK/out.log"
  env EARTH_HOME="$home" GENESIS_SRC="$GEN" "$@" bash "$ENTRYPOINT" >"$LOG" 2>&1
}

# ── 1. join: the default, and the whole point ──────────────────────────────
H="$WORK/join"; mkdir -p "$H"
if run "$H" ; then
  grep -q "matches the release" "$LOG" \
    && ok "join: verifies the genesis hash" \
    || bad "join: did not report a hash match" "$(tail -2 "$LOG")"

  if cmp -s "$GEN" "$H/config/genesis.json"; then
    ok "join: genesis is installed byte-identical"
  else
    bad "join: genesis was modified" "a rewritten file no longer matches its published hash"
  fi

  grep -q "keys add" "$EARTHD_LOG" \
    && bad "join: created a key" "the join path must not mint a validator" \
    || ok "join: creates no keys"

  grep -q "genesis gentx\|collect-gentxs" "$EARTHD_LOG" \
    && bad "join: made its own gentx" "every node would compute a different genesis" \
    || ok "join: makes no gentx"

  grep -q "api.enabled-unsafe-cors" "$EARTHD_LOG" \
    && bad "join: CORS left open" "any origin could read and broadcast" \
    || ok "join: unsafe CORS is off by default"

  grep -q "p2p.laddr tcp://0.0.0.0:26656" "$EARTHD_LOG" \
    && ok "join: p2p binds on all interfaces" \
    || bad "join: p2p not bound" "$(grep STARTED "$EARTHD_LOG" | head -1)"
else
  bad "join: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# ── 2. join with a tampered genesis: must refuse ───────────────────────────
H="$WORK/tampered"; mkdir -p "$H"
BAD_GEN="$WORK/bad.json"; cp "$GEN" "$BAD_GEN"; printf '\n' >> "$BAD_GEN"
cp "$GEN.sha256" "$BAD_GEN.sha256"     # hash of the ORIGINAL, deliberately
export EARTHD_LOG="$WORK/earthd.log"; : > "$EARTHD_LOG"
if env EARTH_HOME="$H" GENESIS_SRC="$BAD_GEN" bash "$ENTRYPOINT" >"$WORK/out.log" 2>&1; then
  bad "tampered: started anyway" "a swapped genesis must not boot"
else
  grep -q "sha256 mismatch" "$WORK/out.log" \
    && ok "tampered: refuses to start, and says why" \
    || bad "tampered: failed for the wrong reason" "$(tail -3 "$WORK/out.log")"
  grep -q "STARTED" "$EARTHD_LOG" \
    && bad "tampered: reached earthd start" "it must fail before starting" \
    || ok "tampered: never reaches start"
fi

# ── 3. DEV_INIT=1: the old behaviour, now opt-in ───────────────────────────
H="$WORK/dev"; mkdir -p "$H"
if run "$H" DEV_INIT=1; then
  grep -q "keys add validator" "$EARTHD_LOG" \
    && ok "dev: creates a validator key" \
    || bad "dev: no key created" "$(tail -3 "$LOG")"
  grep -q "collect-gentxs" "$EARTHD_LOG" \
    && ok "dev: collects a gentx" \
    || bad "dev: no gentx" "$(tail -3 "$LOG")"
  grep -q '"genesis_time": "' "$H/config/genesis.json" \
    && ok "dev: stamps genesis_time" \
    || bad "dev: genesis_time not stamped" "emission would pay out the whole gap"
  cmp -s "$GEN" "$H/config/genesis.json" \
    && bad "dev: genesis unchanged" "the timestamp should have been rewritten" \
    || ok "dev: genesis is modified, so it cannot be mistaken for the release"
else
  bad "dev: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# ── 4. resume: an existing genesis is never replaced ───────────────────────
H="$WORK/resume"; mkdir -p "$H/config"
# The real genesis, not a stub. A resuming node compares what is on its volume
# against what the image ships and refuses to start if they differ, so a fixture
# with a made-up genesis is a fixture for a node that should refuse to start --
# tested separately below. This one stands in for a healthy volume.
cp "$RELEASE_GENESIS" "$H/config/genesis.json"
printf 'priv_validator_laddr = ""\ncors_allowed_origins = []\n' > "$H/config/config.toml"
printf 'snapshot-interval = 1000\nsnapshot-keep-recent = 5\n' > "$H/config/app.toml"
if run "$H"; then
  grep -q "resuming chain" "$LOG" && ok "resume: detects existing state" \
    || bad "resume: did not resume" "$(tail -2 "$LOG")"
  [ "$(shasum -a 256 "$H/config/genesis.json" | awk '{print $1}')" \
    = "$(shasum -a 256 "$RELEASE_GENESIS" | awk '{print $1}')" ] \
    && ok "resume: leaves the existing genesis alone" \
    || bad "resume: overwrote the chain's genesis" "this would fork an existing node"
else
  bad "resume: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# ── 5. CORS is reachable when actually asked for ───────────────────────────
H="$WORK/cors"; mkdir -p "$H"
if run "$H" API_UNSAFE_CORS=1; then
  grep -q "api.enabled-unsafe-cors" "$EARTHD_LOG" \
    && ok "cors: opt-in works" || bad "cors: opt-in ignored" "$(grep STARTED "$EARTHD_LOG")"
  grep -q "WARNING" "$LOG" \
    && ok "cors: warns when enabled" || bad "cors: enabled silently" ""
else
  bad "cors: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# ── 6. RPC CORS allowlist ──────────────────────────────────────────────────
#
# The gap this closes: CosmJS talks to the RPC, not the LCD, and the RPC ships
# with cors_allowed_origins = [] — so a browser has never been able to reach it.
# Confirmed against the live devnet: rpc.erth.network returns no CORS headers at
# all, from the node or from Cloudflare.
H="$WORK/rpccors"; mkdir -p "$H"
if run "$H" RPC_CORS_ORIGINS="https://app.erth.network, https://wallet.erth.network"; then
  want='cors_allowed_origins = ["https://app.erth.network", "https://wallet.erth.network"]'
  got="$(grep '^cors_allowed_origins' "$H/config/config.toml" || true)"
  [ "$got" = "$want" ] \
    && ok "rpc cors: writes a scoped allowlist, trimming whitespace" \
    || bad "rpc cors: wrong TOML" "want: $want
     got:  $got"
else
  bad "rpc cors: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# Unset must leave the default alone — an accidental "*" here is the whole
# problem this is meant to avoid.
H="$WORK/rpccors-off"; mkdir -p "$H"
if run "$H"; then
  grep -q '^cors_allowed_origins = \[\]$' "$H/config/config.toml" \
    && ok "rpc cors: closed unless asked for" \
    || bad "rpc cors: default changed" "$(grep '^cors_allowed_origins' "$H/config/config.toml")"
else
  bad "rpc cors off: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# It is applied on restart too, so an origin can be changed without a wipe.
H="$WORK/rpccors-resume"; mkdir -p "$H/config"
# The real genesis, not a stub. A resuming node compares what is on its volume
# against what the image ships and refuses to start if they differ, so a fixture
# with a made-up genesis is a fixture for a node that should refuse to start --
# tested separately below. This one stands in for a healthy volume.
cp "$RELEASE_GENESIS" "$H/config/genesis.json"
printf 'priv_validator_laddr = ""\ncors_allowed_origins = ["https://old.example"]\n' > "$H/config/config.toml"
printf 'snapshot-interval = 1000\nsnapshot-keep-recent = 5\n' > "$H/config/app.toml"
if run "$H" RPC_CORS_ORIGINS="https://new.example"; then
  grep -q 'cors_allowed_origins = \["https://new.example"\]' "$H/config/config.toml" \
    && ok "rpc cors: re-applied on an existing volume" \
    || bad "rpc cors: stale on restart" "$(grep '^cors_allowed_origins' "$H/config/config.toml")"
else
  bad "rpc cors resume: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# ── 7. state-sync snapshots ────────────────────────────────────────────────
#
# The SDK default is 0, meaning this node offers no snapshots and nobody can
# state-sync from it. A snapshot cannot be produced for a height already passed,
# so launching with it off is a decision that cannot be revisited later.
H="$WORK/snap"; mkdir -p "$H"
if run "$H"; then
  grep -q '^snapshot-interval = 1000$' "$H/config/app.toml" \
    && ok "snapshots: on by default" \
    || bad "snapshots: not enabled" "$(grep '^snapshot-' "$H/config/app.toml")"
  grep -q '^snapshot-keep-recent = 5$' "$H/config/app.toml" \
    && ok "snapshots: keeps 5" \
    || bad "snapshots: wrong keep-recent" "$(grep '^snapshot-keep' "$H/config/app.toml")"
else
  bad "snapshots: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# Overridable, including off — and turning them off should be loud, because a
# node with snapshots off looks identical to one with them on until someone
# tries to sync from it.
H="$WORK/snap-off"; mkdir -p "$H"
if run "$H" SNAPSHOT_INTERVAL=0; then
  grep -q '^snapshot-interval = 0$' "$H/config/app.toml" \
    && ok "snapshots: can be turned off" \
    || bad "snapshots: override ignored" ""
  grep -q "snapshots OFF" "$LOG" \
    && ok "snapshots: says so when off" || bad "snapshots: turned off silently" ""
else
  bad "snapshots off: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# Re-applied on an existing volume, so the cadence can change with a restart.
H="$WORK/snap-resume"; mkdir -p "$H/config"
# The real genesis, not a stub. A resuming node compares what is on its volume
# against what the image ships and refuses to start if they differ, so a fixture
# with a made-up genesis is a fixture for a node that should refuse to start --
# tested separately below. This one stands in for a healthy volume.
cp "$RELEASE_GENESIS" "$H/config/genesis.json"
printf 'priv_validator_laddr = ""\ncors_allowed_origins = []\n' > "$H/config/config.toml"
printf 'snapshot-interval = 1000\nsnapshot-keep-recent = 5\n' > "$H/config/app.toml"
if run "$H" SNAPSHOT_INTERVAL=500; then
  grep -q '^snapshot-interval = 500$' "$H/config/app.toml" \
    && ok "snapshots: re-applied on an existing volume" \
    || bad "snapshots: stale on restart" "$(grep '^snapshot-interval' "$H/config/app.toml")"
else
  bad "snapshots resume: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# ── 8. peering ─────────────────────────────────────────────────────────────
#
# The failure this covers is invisible from inside the container: without
# external_address CometBFT advertises the address it sees on itself, which here
# is a private one, and hands it to every peer through PEX. The node dials out,
# looks healthy, and can never be dialled back.
H="$WORK/peers"; mkdir -p "$H"
if run "$H" EXTERNAL_ADDRESS="203.0.113.7:26656" \
       SEEDS="aaaa@seed.erth.network:26656" \
       PERSISTENT_PEERS="bbbb@peer.one:26656,cccc@peer.two:26656"; then
  grep -q '^external_address = "203.0.113.7:26656"$' "$H/config/config.toml" \
    && ok "peers: advertises the address it was given" \
    || bad "peers: external_address not written" "$(grep '^external_address' "$H/config/config.toml")"
  grep -q '^seeds = "aaaa@seed.erth.network:26656"$' "$H/config/config.toml" \
    && ok "peers: seeds written" || bad "peers: seeds not written" "$(grep '^seeds' "$H/config/config.toml")"
  grep -q '^persistent_peers = "bbbb@peer.one:26656,cccc@peer.two:26656"$' "$H/config/config.toml" \
    && ok "peers: persistent_peers written" \
    || bad "peers: persistent_peers not written" "$(grep '^persistent_peers = ' "$H/config/config.toml")"
  # The name is a prefix of persistent_peers_max_dial_period, which a looser
  # pattern would overwrite with a peer list and leave CometBFT refusing to load.
  grep -q '^persistent_peers_max_dial_period = "0s"$' "$H/config/config.toml" \
    && ok "peers: leaves persistent_peers_max_dial_period alone" \
    || bad "peers: clobbered a neighbouring key" "$(grep '^persistent_peers_max' "$H/config/config.toml")"
  grep -q "advertising 203.0.113.7:26656" "$LOG" \
    && ok "peers: says what it advertises" || bad "peers: silent about its address" ""
else
  bad "peers: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# Unset is the dangerous default, so it has to be loud rather than absent.
H="$WORK/peers-off"; mkdir -p "$H"
if run "$H"; then
  grep -q '^external_address = ""$' "$H/config/config.toml" \
    && ok "peers: no address invented when none is given" \
    || bad "peers: external_address changed unasked" "$(grep '^external_address' "$H/config/config.toml")"
  grep -q "EXTERNAL_ADDRESS unset" "$LOG" \
    && ok "peers: warns that it will be unreachable" \
    || bad "peers: unreachable silently" "the whole failure is that it looks fine"
else
  bad "peers off: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# Applied on restart, so a seed can be added or a wrong address corrected
# without destroying the volume — which on this deployment is the chain.
H="$WORK/peers-resume"; mkdir -p "$H/config"
# The real genesis, not a stub. A resuming node compares what is on its volume
# against what the image ships and refuses to start if they differ, so a fixture
# with a made-up genesis is a fixture for a node that should refuse to start --
# tested separately below. This one stands in for a healthy volume.
cp "$RELEASE_GENESIS" "$H/config/genesis.json"
printf 'priv_validator_laddr = ""\ncors_allowed_origins = []\nexternal_address = "wrong:1"\nseeds = ""\npersistent_peers = ""\n' > "$H/config/config.toml"
printf 'snapshot-interval = 1000\nsnapshot-keep-recent = 5\n' > "$H/config/app.toml"
if run "$H" EXTERNAL_ADDRESS="198.51.100.4:26656" SEEDS="dddd@seed.two:26656"; then
  grep -q '^external_address = "198.51.100.4:26656"$' "$H/config/config.toml" \
    && ok "peers: re-applied on an existing volume" \
    || bad "peers: stale on restart" "$(grep '^external_address' "$H/config/config.toml")"
else
  bad "peers resume: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# ── 9. genesis mismatch: never silently run the wrong chain ────────────────
#
# The case this exists for: a devnet being cut over to a real genesis. The
# volume holds the old chain, the image ships the new one, and the first version
# of this logic resumed the old chain anyway because the volume still carried
# the .devinit-complete marker. The node came up on the old chain holding the
# new consensus key, so it had no voting power and the chain lost its only
# signer -- while everything printed looked healthy.
H="$WORK/mismatch"; mkdir -p "$H/config"
printf '{"not":"the image genesis"}\n' > "$H/config/genesis.json"
# The marker matters: this is a volume that USED to be a devnet and is being cut
# over. Without it here, this test passes against the very bug it exists for.
touch "$H/config/.devinit-complete"
printf 'priv_validator_laddr = ""\ncors_allowed_origins = []\n' > "$H/config/config.toml"
printf 'snapshot-interval = 1000\nsnapshot-keep-recent = 5\n' > "$H/config/app.toml"
if run "$H"; then
  bad "mismatch: started anyway" "resumed a chain that is not the image's"
else
  grep -q "not the one in this image" "$LOG" \
    && ok "mismatch: refuses to start on a foreign genesis" \
    || bad "mismatch: died for the wrong reason" "$(tail -3 "$LOG")"
  grep -q '"not":"the image genesis"' "$H/config/genesis.json" \
    && ok "mismatch: left the volume untouched" \
    || bad "mismatch: destroyed state without being asked" "refusing must not delete"
fi

# Same volume, with the cutover explicitly requested.
if run "$H" RESET_ON_GENESIS_MISMATCH=1; then
  grep -q "DESTROYING the existing chain state" "$LOG" \
    && ok "reset: RESET_ON_GENESIS_MISMATCH=1 discards the old chain" \
    || bad "reset: did not reset" "$(tail -3 "$LOG")"
  [ "$(shasum -a 256 "$H/config/genesis.json" | awk '{print $1}')" \
    = "$(shasum -a 256 "$RELEASE_GENESIS" | awk '{print $1}')" ] \
    && ok "reset: installed the image genesis" \
    || bad "reset: genesis is not the image's" "$(tail -3 "$LOG")"
else
  bad "reset: entrypoint exited non-zero" "$(tail -3 "$LOG")"
fi

# And a DEV_INIT volume must be immune to all of it, or every existing devnet
# deployment breaks on its next image update.
H="$WORK/devnet-immune"; mkdir -p "$H/config"
printf '{"a":"devnet genesis, deliberately unlike the image"}\n' > "$H/config/genesis.json"
printf 'priv_validator_laddr = ""\ncors_allowed_origins = []\n' > "$H/config/config.toml"
printf 'snapshot-interval = 1000\nsnapshot-keep-recent = 5\n' > "$H/config/app.toml"
touch "$H/config/.devinit-complete"
if run "$H" DEV_INIT=1; then
  grep -q '"a":"devnet genesis' "$H/config/genesis.json" \
    && ok "devnet: DEV_INIT=1 volume is never compared or reset" \
    || bad "devnet: a devnet's genesis was replaced" "this breaks every devnet on update"
else
  bad "devnet: DEV_INIT=1 volume failed to start" "$(tail -3 "$LOG")"
fi

# ── state sync, and the cosmovisor slot it depends on ──────────────────────
cat > "$WORK/bin/cosmovisor" <<'CV'
#!/usr/bin/env bash
echo "COSMOVISOR $*" >> "$EARTHD_LOG"
CV
chmod +x "$WORK/bin/cosmovisor"

H="$WORK/statesync"; mkdir -p "$H"
if run "$H" USE_COSMOVISOR=true \
       STATESYNC_RPC_SERVERS="https://rpc.example:443,https://rpc.example:443" \
       STATESYNC_TRUST_HEIGHT=21000 \
       STATESYNC_TRUST_HASH=DEADBEEF; then
  C="$H/config/config.toml"
  grep -q '^trust_height = 21000$' "$C" \
    && ok "statesync: trust_height written unquoted" \
    || bad "statesync: trust_height quoted or missing" "$(grep '^trust_height' "$C")"
  grep -q '^trust_hash = "DEADBEEF"$' "$C" \
    && ok "statesync: trust_hash written quoted" \
    || bad "statesync: trust_hash wrong" "$(grep '^trust_hash' "$C")"
  awk '/^\[statesync\]/{s=1} s&&/^enable = /{print; exit}' "$C" | grep -q 'true' \
    && ok "statesync: enabled in its own section" \
    || bad "statesync: not enabled" "$(grep -n '^enable' "$C")"
  awk '/^\[tx_index\]/{s=1} s&&/^enable = /{print; exit}' "$C" | grep -q 'false' \
    && ok "statesync: left another section's enable alone" \
    || bad "statesync: clobbered [tx_index] enable" "$(grep -n '^enable' "$C")"
  if [ -L "$H/cosmovisor/current" ]; then
    case "$(readlink "$H/cosmovisor/current")" in
      *upgrades/v9.9.9) ok "statesync: image binary runs as cosmovisor current" ;;
      *) bad "statesync: current points somewhere unexpected" "$(readlink "$H/cosmovisor/current")" ;;
    esac
  else
    bad "statesync: no cosmovisor/current" "the node would start on the launch binary and diverge"
  fi
else
  bad "statesync: entrypoint failed" "$(tail -3 "$LOG")"
fi

H="$WORK/replay"; mkdir -p "$H"
if run "$H" USE_COSMOVISOR=true; then
  [ -L "$H/cosmovisor/genesis/bin/earthd" ] \
    && ok "replay: image binary is the genesis binary" \
    || bad "replay: genesis/bin not populated"
  [ -L "$H/cosmovisor/current" ] \
    && bad "replay: current was set" "cosmovisor would not walk the upgrades" \
    || ok "replay: no current, so cosmovisor walks each upgrade in turn"
else
  bad "replay: entrypoint failed" "$(tail -3 "$LOG")"
fi

H="$WORK/notrust"; mkdir -p "$H"
if run "$H" USE_COSMOVISOR=true STATESYNC_RPC_SERVERS="https://rpc.example:443"; then
  bad "statesync: started with no trust height or hash"
else
  grep -q "STATESYNC_TRUST_HEIGHT" "$LOG" \
    && ok "statesync: refuses rpc_servers without a trust height and hash" \
    || bad "statesync: died for the wrong reason" "$(tail -3 "$LOG")"
fi

# REQUIRE_NO_CONSENSUS_KEY: the guard against a volume that was once a validator
# signing again from the key still on its disk. See the entrypoint for the fork
# this exists to prevent.
H="$WORK/keyless-clean"; mkdir -p "$H"
if run "$H" REQUIRE_NO_CONSENSUS_KEY=1; then
  ok "keyless: starts when the volume really has no consensus key"
else
  bad "keyless: refused a genuinely keyless volume" "$(tail -3 "$LOG")"
fi

H="$WORK/keyless-dirty"; mkdir -p "$H/config"
printf '{"address":"AA","priv_key":{"type":"tendermint/PrivKeyEd25519","value":"x"}}' \
  > "$H/config/priv_validator_key.json"
if run "$H" REQUIRE_NO_CONSENSUS_KEY=1; then
  bad "keyless: started on a volume that still holds a consensus key" \
      "this is the 69-block fork of 2026-09-01"
else
  grep -q "priv_validator_key.json already exists" "$LOG" \
    && ok "keyless: refuses a volume that still holds a consensus key" \
    || bad "keyless: died for the wrong reason" "$(tail -3 "$LOG")"
fi

# BD-5 (final audit): init and start both write a random key, so a keyless node
# must still come up on a fresh volume and keep coming up on restarts.
H="$WORK/keyless-restart"; mkdir -p "$H"
run "$H" REQUIRE_NO_CONSENSUS_KEY=1 || true
if run "$H" REQUIRE_NO_CONSENSUS_KEY=1; then
  grep -q "recorded throwaway" "$LOG" \
    && ok "keyless: restarts on its own recorded throwaway key" \
    || bad "keyless: restart passed for the wrong reason" "$(tail -3 "$LOG")"
else
  bad "keyless: a keyless node cannot restart" "$(tail -3 "$LOG")"
fi
# The throwaway swapped for another key between boots: refused.
printf '{"address":"AA","pub_key":{"type":"tendermint/PubKeyEd25519","value":"other"},"priv_key":{"value":"x"}}' \
  > "$H/config/priv_validator_key.json"
if run "$H" REQUIRE_NO_CONSENSUS_KEY=1; then
  bad "keyless: accepted a key the throwaway mark does not name"
else
  grep -q "not the throwaway this guard recorded" "$LOG" \
    && ok "keyless: refuses a key that replaced the recorded throwaway" \
    || bad "keyless: died for the wrong reason" "$(tail -3 "$LOG")"
fi
# The operator moved a key aside on an existing volume: a throwaway is minted.
mv "$H/config/priv_validator_key.json" "$H/config/priv_validator_key.json.REMOVED"
if run "$H" REQUIRE_NO_CONSENSUS_KEY=1 && run "$H" REQUIRE_NO_CONSENSUS_KEY=1; then
  ok "keyless: a key moved aside is replaced by a recorded throwaway, restart-stable"
else
  bad "keyless: moving the key aside did not give a startable node" "$(tail -3 "$LOG")"
fi
# A key the genesis gentx names is a validator key, mark or no mark.
H="$WORK/keyless-genesis-key"; mkdir -p "$H"
run "$H" REQUIRE_NO_CONSENSUS_KEY=1 || true
GPUB="$(python3 -c 'import json,sys;g=json.load(open(sys.argv[1]));print(g["app_state"]["genutil"]["gen_txs"][0]["body"]["messages"][0]["pubkey"]["key"])' "$GEN")"
printf '{"address":"AA","pub_key":{"type":"tendermint/PubKeyEd25519","value":"%s"},"priv_key":{"value":"x"}}' "$GPUB" \
  > "$H/config/priv_validator_key.json"
sha256_f() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }
sha256_f "$H/config/priv_validator_key.json" > "$H/config/.keyless-throwaway-key.sha256"
if run "$H" REQUIRE_NO_CONSENSUS_KEY=1; then
  bad "keyless: started holding the genesis validator's key"
else
  grep -q "named in the genesis" "$LOG" \
    && ok "keyless: refuses the genesis validator's key even if marked" \
    || bad "keyless: died for the wrong reason" "$(tail -3 "$LOG")"
fi

H="$WORK/keyless-contradiction"; mkdir -p "$H"
if run "$H" REQUIRE_NO_CONSENSUS_KEY=1 PRIV_VALIDATOR_KEY_B64=Zm9v; then
  bad "keyless: accepted REQUIRE_NO_CONSENSUS_KEY alongside a key to inject"
else
  grep -q "These contradict" "$LOG" \
    && ok "keyless: refuses REQUIRE_NO_CONSENSUS_KEY plus PRIV_VALIDATOR_KEY_B64" \
    || bad "keyless: died for the wrong reason" "$(tail -3 "$LOG")"
fi

printf '\n  %d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
