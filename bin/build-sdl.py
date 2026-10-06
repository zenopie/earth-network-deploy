#!/usr/bin/env python3
"""Build the SDL that actually gets submitted: akash/deploy.yaml + the image
digest for a released tag + secrets from .env.

Textual insertion, not a YAML round-trip, so the committed file's comments and
exact shape survive into what the provider receives. A round-trip would also
risk turning version: "2.0" into a float, which the SDL parser rejects.
"""
import argparse
import base64
import hashlib
import json
import os, re, sys, yaml

ap = argparse.ArgumentParser()
ap.add_argument("repo")                       # this repo
ap.add_argument("out")                        # where to write the submitted copy
ap.add_argument("digest", nargs="?")          # ghcr.io/...@sha256:...
ap.add_argument("--sdl", default="akash/deploy.yaml",
                help="which SDL to build, relative to the repo")
ap.add_argument("--validator-key", action="store_true",
                help="with --fullnode: inject PRIV_VALIDATOR_KEY_B64, the key "
                     "that can double-sign. MUST NOT be submitted while any other "
                     "node holding that key is running earthd.")
ap.add_argument("--tunnel", action="store_true",
                help="with --fullnode: inject the real TUNNEL_TOKEN instead of the "
                     "placeholder, so this node starts serving rpc/lcd.")
ap.add_argument("--no-statesync", action="store_true",
                help="with --fullnode: this node does not state sync. Either it "
                     "already holds the data (resuming a volume) or it replays "
                     "from block 1 with the binaries driven by hand across the "
                     "upgrade heights. Relaxes only the state-sync assertion; "
                     "every no-consensus-key guarantee still applies.")
ap.add_argument("--node-key", action="store_true",
                help="with --fullnode --validator-key: also inject NODE_KEY_B64, so "
                     "a validator on a NEW lease has the p2p id joiners were told "
                     "to dial, from its first boot. Only "
                     "with --validator-key: a second node carrying the same node "
                     "key is two peers claiming one id.")
ap.add_argument("--relayer", action="store_true",
                help="with --fullnode: inject RELAYER_MNEMONIC into the relayer "
                     "service. Opt-in: refused unless the SDL sets the relayer's "
                     "ENABLED=true, so the key never ships to a lease that does "
                     "not run it.")
ap.add_argument("--genesis",
                help="with --validator-key: a genesis.json (e.g. the launch tag's "
                     "release asset) to check the key against. It must hash to "
                     "akash/genesis.sha256 and its gentx must name this key.")
ap.add_argument("--fullnode", action="store_true",
                help="required. Keeps VALIDATOR_MNEMONIC and RELAYER_MNEMONIC out "
                     "of the SDL; with nothing else it builds a node with NO "
                     "consensus key, no NODE_KEY_B64 and a tunnel token "
                     "placeholder. The validator adds --validator-key, "
                     "--node-key and --tunnel.")
args = ap.parse_args()
repo, out, digest, fullnode = args.repo, args.out, args.digest, args.fullnode
want_valkey, want_tunnel = args.validator_key, args.tunnel
want_nodekey = args.node_key
want_relayer = args.relayer
assert not args.genesis or want_valkey, "--genesis only applies with --validator-key"
assert not want_nodekey or (fullnode and want_valkey), \
    "--node-key only applies with --fullnode --validator-key"

# The chain id every SDL here must carry: the relaunch genesis's (chain repo
# networks/genesis/chain.json). Kept as earth-1; the new consensus key, not a new
# id, is what keeps the old chain's signatures from counting against this one.
EXPECTED_CHAIN_ID = "earth-1"
# The consensus pubkey the launch genesis's one gentx names (RELAUNCH.md, "Where
# things stand"; chain repo scripts/ceremony.sh --pubkey). --validator-key
# refuses any other key. Change it only together with a new gentx.
EXPECTED_CONSENSUS_PUBKEY = "PGqvPN4CxEkxvvh3tSBX0SGeBgjMdqQwZkdHt8FRLm4="
# Keys that signed an earlier earth-1. Their signatures at the same heights
# under the same chain id are double-sign evidence: never again (chain repo
# scripts/ceremony.sh USED_CONSENSUS_KEYS).
USED_CONSENSUS_PUBKEYS = {"kTMzoCBEj1g2z49K1D/jxuLGrhTsnzfTx6Gf1LnBUJw="}
no_statesync = args.no_statesync
# Without --fullnode this script used to inject VALIDATOR_MNEMONIC after a
# VALIDATOR_BONDED line for a DEV_INIT devnet. akash/deploy.yaml has no such line
# on purpose, so the mnemonic never reaches a lease: refuse up front.
if not fullnode:
    sys.exit("build-sdl.py: pass --fullnode (the validator is built with "
             "--fullnode --no-statesync --validator-key --node-key --tunnel; "
             "see RELAUNCH.md, section 3)")
assert not no_statesync or fullnode, \
    "--no-statesync only applies on top of --fullnode"
assert not (want_valkey or want_tunnel) or fullnode, \
    "--validator-key and --tunnel only apply on top of --fullnode"

# A cloudflared that cannot authenticate is the point, not an oversight: the
# sync node must NOT attach a replica to the live tunnel, or Cloudflare
# load-balances real traffic onto a node that is still catching up. cloudflared
# restart-loops on this, harmlessly, until the real token is put in by an
# in-place env PUT at swap time.
TUNNEL_PLACEHOLDER = "PLACEHOLDER-not-a-token-set-at-tunnel-swap"

env = {}
for line in open(os.path.join(repo, ".env")):
    line = line.strip()
    if line and not line.startswith("#") and "=" in line:
        k, v = line.split("=", 1)
        v = v.strip()
        # Strip surrounding quotes the way `source .env` would. Without this the
        # quotes travel into the SDL as part of the value, and the container gets
        # a mnemonic that starts with a double quote -- which BIP39 rejects with
        # "invalid mnemonic", killing DEV_INIT at key recovery.
        if len(v) >= 2 and v[0] == v[-1] and v[0] in ("'", '"'):
            v = v[1:-1]
        env[k.strip()] = v

s = open(os.path.join(repo, args.sdl)).read()

# Pin the image at deploy time rather than at build time. The chain repo used to
# rewrite this and commit it back; it no longer knows this file exists.
if digest:
    s, n = re.subn(r'(?m)^(\s*image:\s*)ghcr\.io/\S+', r'\1' + digest, s)
    assert n, "no ghcr image line to pin"
    print("pinned %d image line(s) to %s" % (n, digest.split("@")[-1][:19] + "…"))

# A --fullnode SDL without --validator-key describes a node that must not be
# able to sign. Saying so in the SDL is not enough: the entrypoint only ever
# OVERWRITES priv_validator_key.json, so a volume that was once a validator
# still has one and CometBFT will sign from it. That produced a 69-block fork on
# 2026-09-01. REQUIRE_NO_CONSENSUS_KEY=1 makes the container refuse to start if
# such a file is present, turning a silent fork into a loud halt.
if fullnode and not want_valkey:
    guard_anchor = "      - DEV_INIT=0\n"
    assert s.count(guard_anchor) == 1, "DEV_INIT anchor moved"
    s = s.replace(guard_anchor, guard_anchor + "      - REQUIRE_NO_CONSENSUS_KEY=1\n")

# The consensus key the genesis gentx names (and, with --node-key, the node key
# that fixes the peer id). Base64 in .env, passed through verbatim -- the
# entrypoint decodes them; raw JSON here would be a YAML flow mapping. DEV_INIT=0
# creates no account, so the mnemonic stays out. PRIV_VALIDATOR_KEY_B64 can
# double-sign: a validator with real stake should move to PRIV_VALIDATOR_LADDR
# and a remote signer (akash/REMOTE_SIGNER.md).
if fullnode and want_valkey:
    key_anchor = "      - DEV_INIT=0\n"
    assert s.count(key_anchor) == 1, "DEV_INIT anchor moved"
    assert env.get("PRIV_VALIDATOR_KEY_B64"), "no PRIV_VALIDATOR_KEY_B64 in .env to promote with"
    s = s.replace(key_anchor,
                  key_anchor + f"      - PRIV_VALIDATOR_KEY_B64={env['PRIV_VALIDATOR_KEY_B64']}\n")
    if want_nodekey:
        assert env.get("NODE_KEY_B64"), "no NODE_KEY_B64 in .env for --node-key"
        s = s.replace(key_anchor,
                      key_anchor + f"      - NODE_KEY_B64={env['NODE_KEY_B64']}\n")

# cloudflared: the tunnel token replaces the empty env list
anchor = "    env: []\n"
assert s.count(anchor) == 1, "cloudflared env anchor moved"
use_placeholder = fullnode and not want_tunnel
s = s.replace(anchor, "    env:\n      - TUNNEL_TOKEN=%s\n"
              % (TUNNEL_PLACEHOLDER if use_placeholder else env["TUNNEL_TOKEN"]))

# relayer: its key, only with --relayer (BD-9). Injected the same way and for
# the same reason as the other two — it reaches the provider either way, what
# this avoids is it reaching a public repository. Opt-in, and only when the
# relayer is switched on (checked below): the key pays gas and holds nothing
# else, but "low value" is not a reason to spread it.
anchor = "      - LINK_ON_START="
if want_relayer:
    assert anchor in s, "--relayer: no LINK_ON_START line to anchor RELAYER_MNEMONIC on"
    assert env.get("RELAYER_MNEMONIC"), "--relayer: no RELAYER_MNEMONIC in .env"
    line = s[s.index(anchor):]
    line = line[:line.index("\n") + 1]
    s = s.replace(line, line + f"      - RELAYER_MNEMONIC={env['RELAYER_MNEMONIC']}\n")

open(out, "w").write(s)

# Verify what we built rather than trusting the string edits.
d = yaml.safe_load(s)
assert d["version"] == "2.0", f'version became {d["version"]!r}'
svcs = d["services"]
assert set(svcs) == {"node", "relayer", "cloudflared"}, sorted(svcs)

def envmap(name):
    if name not in svcs:
        return {}
    return dict(e.split("=", 1) for e in (svcs[name].get("env") or []))

n, c, r = envmap("node"), envmap("cloudflared"), envmap("relayer")
# A fresh volume has to end up with a validator signing blocks, and there are
# exactly two ways for that to happen. DEV_INIT=1 builds a throwaway chain and
# makes its own. DEV_INIT=0 joins the image's genesis, which only produces
# blocks if that genesis already names a validator AND this node holds the
# matching consensus key. Asserting DEV_INIT=1 outright was right while the
# release genesis had no gentx; now it would forbid the correct configuration.
if fullnode:
    # The inverse of the check below, and the reason --fullnode exists. A second
    # node that came up holding the live validator's consensus key would sign at
    # heights the first node is also signing: equivocation, on a chain where one
    # validator holds all the voting power.
    for var in ("VALIDATOR_MNEMONIC",) + (() if want_nodekey else ("NODE_KEY_B64",)):
        assert var not in n, (
            f"--fullnode built an SDL carrying {var}. This node must hold no "
            "signing identity; refusing to submit one that does.")
    if want_valkey:
        assert n.get("PRIV_VALIDATOR_KEY_B64"), "--validator-key injected nothing"
        try:
            pvk = json.loads(base64.b64decode(n["PRIV_VALIDATOR_KEY_B64"]))
        except Exception as e:
            raise AssertionError("PRIV_VALIDATOR_KEY_B64 is not base64 of JSON: %s" % e)
        assert "priv_key" in pvk and "address" in pvk and "pub_key" in pvk, \
            "PRIV_VALIDATOR_KEY_B64 is not a priv_validator_key.json"
        # BD-11: the key must be the one the genesis names, and never one that
        # signed an earlier earth-1. A swapped .env otherwise brings up a
        # validator with no power, or re-signs old heights with the old key.
        pub = pvk["pub_key"].get("value", "")
        assert pub not in USED_CONSENSUS_PUBKEYS, (
            "PRIV_VALIDATOR_KEY_B64 is a consensus key that signed an earlier "
            "earth-1 (%s…). It must never sign again under this chain id." % pub[:8])
        assert pub == EXPECTED_CONSENSUS_PUBKEY, (
            "PRIV_VALIDATOR_KEY_B64 pubkey %s… is not the launch gentx's %s…"
            % (pub[:8], EXPECTED_CONSENSUS_PUBKEY[:8]))
        raw = base64.b64decode(pub)
        addr = hashlib.sha256(raw).hexdigest()[:40].upper()
        assert pvk["address"].upper() == addr, (
            "priv_validator_key.json address %s does not derive from its pubkey "
            "(%s): the file was edited" % (pvk["address"], addr))
        if args.genesis:
            graw = open(args.genesis, "rb").read()
            pin = open(os.path.join(repo, "akash/genesis.sha256")).read().split()[0]
            assert hashlib.sha256(graw).hexdigest() == pin, (
                "--genesis %s does not hash to akash/genesis.sha256" % args.genesis)
            gtx = json.loads(graw)["app_state"]["genutil"]["gen_txs"]
            gpubs = [m.get("pubkey", {}).get("key") for t in gtx for m in t["body"]["messages"]]
            assert pub in gpubs, (
                "the pinned genesis's gentx names %s, not this key" % gpubs)
    else:
        assert "PRIV_VALIDATOR_KEY_B64" not in n, (
            "--fullnode built an SDL carrying PRIV_VALIDATOR_KEY_B64 without "
            "--validator-key. Two nodes holding this key sign the same heights, "
            "which on a one-validator chain is self-slashing equivocation.")
        assert n.get("REQUIRE_NO_CONSENSUS_KEY") == "1", (
            "keyless --fullnode SDL without REQUIRE_NO_CONSENSUS_KEY=1: the SDL "
            "carrying no key does not mean the volume carries no key")
    assert n.get("DEV_INIT") == "0", "--fullnode must join an existing chain, not init one"
    if not no_statesync:
        assert n.get("STATESYNC_RPC_SERVERS"), (
            "--fullnode with no STATESYNC_RPC_SERVERS. Pass --no-statesync: "
            "this SDL is the full-history node, which never state syncs (and "
            "the check below refuses STATESYNC_RPC_SERVERS outright).")
    else:
        # A full-history node starts its volume at block 1 (or resumes one it
        # already filled). This is the ONLY thing --no-statesync relaxes; every
        # no-signing-identity guarantee above still applies.
        assert not n.get("STATESYNC_RPC_SERVERS"), (
            "--no-statesync with STATESYNC_RPC_SERVERS set: state sync would "
            "jump this node to a trusted height and it would never hold the "
            "early blocks that are the whole point of replaying.")
    # Present, but allowed to be empty. A node that has to reach the network
    # needs peers, and forgetting them is the mistake this catches. But
    # earth-1 runs ONE node -- the validator, which is also the full-history
    # RPC node behind rpc/lcd.erth.network -- so there is nobody to dial, and naming a dead peer only
    # produces a reconnect error every few seconds. An explicitly empty value
    # says "deliberately alone"; a missing key still says "you forgot".
    assert "PERSISTENT_PEERS" in n, (
        "--fullnode with no PERSISTENT_PEERS key: set it, empty if this node is "
        "deliberately the only one on the network")
    if want_relayer:
        assert r.get("ENABLED") == "true", (
            "--relayer with the relayer's ENABLED not true: the key would ship to "
            "a lease that never uses it")
    else:
        assert "RELAYER_MNEMONIC" not in r, (
            "SDL carries RELAYER_MNEMONIC without --relayer; the key has no "
            "business reaching a provider that does not run the relayer")

# An SDL with a <placeholder> left in it would boot a node that dials, or
# advertises, nothing real. Fill it in or comment the line out.
for svc in svcs:
    for k, v in envmap(svc).items():
        assert not ("<" in v and ">" in v), f"{svc}: {k}={v} is an unfilled placeholder"

assert n.get("CHAIN_ID") == EXPECTED_CHAIN_ID, (n.get("CHAIN_ID"), EXPECTED_CHAIN_ID)

# BD-1: RESET_ON_GENESIS_MISMATCH=1 wipes config/ and data/ when the volume's
# genesis differs from the image's. On the only node that is the whole chain.
# Refused outright, whatever its value: a mismatch must stop the node, not
# reset it.
assert "RESET_ON_GENESIS_MISMATCH" not in n, (
    "RESET_ON_GENESIS_MISMATCH is set: on earth-1's only node it can destroy all "
    "chain state on one image bump. Remove it (final audit BD-1).")

# Private (unsigned) txs need the SDK's no-op app mempool; any max-txs >= 0
# rejects them in CheckTx. Every node on this chain, every SDL.
assert n.get("EARTHD_MEMPOOL_MAX_TXS") == "-1", (
    "EARTHD_MEMPOOL_MAX_TXS must be -1: the priority and sender-nonce mempools "
    "reject zero-signer txs, i.e. every claim, vote and private transfer")
assert "uanml" not in n.get("MIN_GAS_PRICES", ""), \
    "ANML is shielded-only and refused as a fee; MIN_GAS_PRICES is uerth only"

# The one node is also the full-history node the privacy indexer reads. Its
# job includes every block and every block's results from height 1, which the
# indexer downloads in full ranges through rpc.erth.network; a gap is an index
# wallets cannot use, and it can never be refilled on this volume.
for k, want in (("EARTHD_PRUNING", "nothing"),
                ("EARTHD_MIN_RETAIN_BLOCKS", "0"),
                ("EARTHD_STORAGE_DISCARD_ABCI_RESPONSES", "false"),
                ("EARTHD_TX_INDEX_INDEXER", "kv"),
                # The data volume grows without bound; cosmovisor's pre-upgrade
                # copy of data/ is what filled the last chain's disk and halted it.
                ("UNSAFE_SKIP_BACKUP", "true")):
    assert n.get(k) == want, f"the full-history node needs {k}={want}, has {n.get(k)!r}"
# BD-6: the public RPC/LCD is served by the only signer. Bound what one burst
# of queries can take from consensus (per-IP limits are at Cloudflare).
qgl = n.get("EARTHD_QUERY_GAS_LIMIT", "0")
assert qgl.isdigit() and int(qgl) > 0, "EARTHD_QUERY_GAS_LIMIT must be set and > 0 (0 is unbounded)"
for k, cap in (("EARTHD_RPC_MAX_OPEN_CONNECTIONS", 200),
               ("EARTHD_API_MAX_OPEN_CONNECTIONS", 400)):
    v = n.get(k, "")
    assert v.isdigit() and 0 < int(v) <= cap, f"{k} must be set to 1..{cap}, has {v!r}"
# R3-BD-1: no subscriptions at all. Nothing of ours subscribes, and a socket
# is how tx_search got past Cloudflare's URI checks; Cloudflare now blocks
# /websocket, and this is the node's own half of that.
assert n.get("EARTHD_RPC_MAX_SUBSCRIPTION_CLIENTS") == "0", (
    "EARTHD_RPC_MAX_SUBSCRIPTION_CLIENTS must be 0 (akash/README.md, rule 1)")
assert n.get("EARTHD_RPC_UNSAFE") == "false", "EARTHD_RPC_UNSAFE must be false"
assert not n.get("STATESYNC_RPC_SERVERS"), (
    "STATESYNC_RPC_SERVERS set: state sync floors the node at its trust height "
    "and the indexer would have a permanent hole below it")
if want_tunnel:
    assert c.get("TUNNEL_TOKEN", "").startswith("eyJ"), "--tunnel injected no real token"
else:
    assert c.get("TUNNEL_TOKEN") == TUNNEL_PLACEHOLDER, (
        "--fullnode must not carry the live tunnel token: a second replica on "
        "that tunnel splits real traffic onto a node that is still syncing")
if r and r.get("ENABLED") == "true":
    rm = r.get("RELAYER_MNEMONIC", "")
    assert len(rm.split()) in (12, 24), "relayer enabled but its mnemonic is missing/malformed"
    assert not any(c in rm for c in "\"'"), "relayer mnemonic carries quote characters"
    for k in ("COUNTERPARTY_CHAIN_ID", "COUNTERPARTY_RPC", "COUNTERPARTY_PREFIX"):
        assert r.get(k), f"relayer enabled but {k} is unset"

# cloudflared holds TUNNEL_TOKEN: pinned by digest, and its metrics server
# (which also serves /debug/pprof, /config and /diag/*) on loopback only (BD-4,
# the backend's audit-6 L3).
cf = svcs["cloudflared"]
assert "@sha256:" in cf["image"], "cloudflared image %s is not pinned by digest" % cf["image"]
cmd = cf.get("command") or []
for i, arg in enumerate(cmd):
    if arg == "--metrics" or arg.startswith("--metrics="):
        addr = arg.split("=", 1)[1] if "=" in arg else (cmd[i + 1] if i + 1 < len(cmd) else "")
        assert addr.startswith("127.0.0.1:") or addr.startswith("localhost:"), (
            "cloudflared --metrics %s is not loopback: it serves pprof and /config" % addr)

# NO_LOGS.md: nothing that records client requests. cloudflared at debug/trace
# logs request headers (CF-Connecting-IP); earthd at debug logs every RPC
# request and its remote address.
# Every spelling cloudflared 2026.9.3 accepts (cmd/cloudflared/cliutil/logger.go):
# --proto-loglevel is the old name of --transport-loglevel; --trace-output
# writes a runtime trace to a file; a --config file can set any of these where
# the checks below cannot see it (R3-BD-7).
LEVEL_FLAGS = ("--loglevel", "--transport-loglevel", "--proto-loglevel")
FILE_FLAGS = ("--logfile", "--log-directory", "--trace-output", "--config")
for i, arg in enumerate(cmd):
    if arg in LEVEL_FLAGS or arg.startswith(tuple(f + "=" for f in LEVEL_FLAGS)):
        lvl = arg.split("=", 1)[1] if "=" in arg else (cmd[i + 1] if i + 1 < len(cmd) else "")
        assert lvl not in ("debug", "trace"), "cloudflared %s %s logs client requests" % (arg, lvl)
    assert not (arg in FILE_FLAGS or arg.startswith(tuple(f + "=" for f in FILE_FLAGS))), (
        "cloudflared %s writes logs, traces or settings to or from disk (NO_LOGS.md)" % arg)
# cloudflared also reads each flag from a TUNNEL_* env var, which the command
# checks above never see; the command is the one place these are set.
for k in sorted(c):
    assert k not in ("TUNNEL_LOGLEVEL", "TUNNEL_TRANSPORT_LOGLEVEL", "TUNNEL_PROTO_LOGLEVEL", "TUNNEL_LOGFILE",
                     "TUNNEL_LOGDIRECTORY", "TUNNEL_TRACE_OUTPUT", "TUNNEL_METRICS"), (
        "cloudflared env %s is refused: set it in command, where it is checked" % k)
ll = n.get("EARTHD_LOG_LEVEL", "")
assert ll, "EARTHD_LOG_LEVEL unset: pin it (NO_LOGS.md)"
assert not any(w in ll for w in ("debug", "trace")), "EARTHD_LOG_LEVEL=%s logs client requests" % ll
assert "rpc-server:error" in ll, "EARTHD_LOG_LEVEL must hold rpc-server at error (websocket remote addresses)"

img = svcs["node"]["image"]
if "relayer" in svcs:
    assert img == svcs["relayer"]["image"], "node and relayer images differ"
print("services:   ", ", ".join(sorted(svcs)))
print("node image: ", img)
print("DEV_INIT:   ", n["DEV_INIT"], " CHAIN_ID:", n["CHAIN_ID"], " MIN_GAS:", n.get("MIN_GAS_PRICES"),
      " mempool.max-txs:", n.get("EARTHD_MEMPOOL_MAX_TXS"))
print("limits:       query_gas=%s rpc_conns=%s rpc_subs=%s api_conns=%s rpc_unsafe=%s" % (
      n["EARTHD_QUERY_GAS_LIMIT"], n["EARTHD_RPC_MAX_OPEN_CONNECTIONS"],
      n["EARTHD_RPC_MAX_SUBSCRIPTION_CLIENTS"], n["EARTHD_API_MAX_OPEN_CONNECTIONS"],
      n["EARTHD_RPC_UNSAFE"]))
print("history:      pruning=%s discard_abci_responses=%s tx_index=%s skip_backup=%s" % (
      n["EARTHD_PRUNING"], n["EARTHD_STORAGE_DISCARD_ABCI_RESPONSES"],
      n["EARTHD_TX_INDEX_INDEXER"], n["UNSAFE_SKIP_BACKUP"]))
if n.get("NODE_KEY_B64"):
    # node id = hex of the first 20 bytes of sha256(ed25519 pubkey); the pubkey
    # is the last 32 bytes of priv_key.value. Printed so the id
    # joiners dial can be checked against what this lease will present.
    nk = json.loads(base64.b64decode(n["NODE_KEY_B64"]))
    pub = base64.b64decode(nk["priv_key"]["value"])[32:]
    print("node id:      %s (from NODE_KEY_B64)" % hashlib.sha256(pub).hexdigest()[:40])
if want_valkey:
    # Public halves only: the address and pubkey the genesis gentx must name.
    kd = json.loads(base64.b64decode(n["PRIV_VALIDATOR_KEY_B64"]))
    print("consensus:   %s pubkey %s (from PRIV_VALIDATOR_KEY_B64)"
          % (kd["address"], kd["pub_key"].get("value")))
    print("external:    %s" % n.get("EXTERNAL_ADDRESS", "UNSET - peers cannot dial this node"))
print("MODE:        --fullnode%s%s%s" % (
    " +NO-STATESYNC" if no_statesync else "",
    " +VALIDATOR-KEY" if want_valkey else " — no consensus key",
    " +TUNNEL" if want_tunnel else " — tunnel placeholder"))
# Which it is depends on the volume, which this script cannot see.
print("no statesync: resumes its volume, or starts from block 1 if empty"
      " — cosmovisor=%s" % n.get("USE_COSMOVISOR", "false"))
print("peers:       %s" % (n["PERSISTENT_PEERS"] or "(none — solo node)"))
if r:
    print("relayer:     ENABLED=%s%s" % (r["ENABLED"],
          "  -> " + r.get("COUNTERPARTY_CHAIN_ID", "") + "  link=" + r.get("LINK_ON_START", "false")
          if r["ENABLED"] == "true" else ""))
else:
    print("relayer:     (none in this SDL)")
print("wrote", out, "(%d bytes)" % len(s))
