#!/usr/bin/env python3
"""Build the SDL that actually gets submitted: akash/deploy.yaml + the image
digest for a released tag + secrets from .env.

Textual insertion, not a YAML round-trip, so the committed file's comments and
exact shape survive into what the provider receives. A round-trip would also
risk turning version: "2.0" into a float, which the SDL parser rejects.
"""
import argparse
import base64
import json
import os, re, sys, yaml

ap = argparse.ArgumentParser()
ap.add_argument("repo")                       # this repo
ap.add_argument("out")                        # where to write the submitted copy
ap.add_argument("digest", nargs="?")          # ghcr.io/...@sha256:...
ap.add_argument("--sdl", default="akash/deploy.yaml",
                help="which SDL to build, relative to the repo")
ap.add_argument("--validator-key", action="store_true",
                help="with --fullnode: inject PRIV_VALIDATOR_KEY_B64 and nothing "
                     "else. This is the swap — it turns the sync node into the "
                     "validator, and MUST NOT be submitted while the old node is "
                     "still running earthd.")
ap.add_argument("--tunnel", action="store_true",
                help="with --fullnode: inject the real TUNNEL_TOKEN instead of the "
                     "placeholder, so this node starts serving rpc/lcd.")
ap.add_argument("--no-statesync", action="store_true",
                help="with --fullnode: this node does not state sync. Either it "
                     "already holds the data (resuming a volume) or it replays "
                     "from block 1 with the binaries driven by hand across the "
                     "upgrade heights. Relaxes only the state-sync assertion; "
                     "every no-consensus-key guarantee still applies.")
ap.add_argument("--fullnode", action="store_true",
                help="build a node with NO consensus key: no validator mnemonic, "
                     "no PRIV_VALIDATOR_KEY_B64, no NODE_KEY_B64, and a tunnel "
                     "token placeholder. For a second node that joins the "
                     "network to sync rather than to sign.")
args = ap.parse_args()
repo, out, digest, fullnode = args.repo, args.out, args.digest, args.fullnode
want_valkey, want_tunnel = args.validator_key, args.tunnel
no_statesync = args.no_statesync
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

# node: the devnet validator key, appended after the last VALIDATOR_* line
if not fullnode:
    anchor = "      - VALIDATOR_BONDED=100000000uerth\n"
    assert s.count(anchor) == 1, "VALIDATOR_BONDED anchor moved"
    s = s.replace(anchor, anchor + f"      - VALIDATOR_MNEMONIC={env['VALIDATOR_MNEMONIC']}\n")

# node: the consensus key named in the genesis gentx, and the node key that
# fixes the peer id. Base64 in .env, passed through verbatim -- the entrypoint
# decodes them. Raw JSON here would be a YAML flow mapping, not a string.
#
# PRIV_VALIDATOR_KEY_B64 is the key that can double-sign. It is in this file for
# a single-validator devnet whose whole state is disposable; a validator with
# real stake should use PRIV_VALIDATOR_LADDR and a remote signer instead
# (akash/REMOTE_SIGNER.md).
# In --fullnode this loop is skipped entirely. Both keys are identity: the
# consensus key is the one that can double-sign, and reusing the node key would
# put two peers on the network claiming the same id.
if not fullnode:
    for var in ("PRIV_VALIDATOR_KEY_B64", "NODE_KEY_B64"):
        if env.get(var):
            s = s.replace(anchor, anchor + f"      - {var}={env[var]}\n")

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

# The swap. Only the consensus key: the node keeps the p2p identity it synced
# with, and DEV_INIT=0 means no account needs creating, so the mnemonic stays
# out. One secret, added at one moment, for one reason.
if fullnode and want_valkey:
    key_anchor = "      - DEV_INIT=0\n"
    assert s.count(key_anchor) == 1, "DEV_INIT anchor moved"
    assert env.get("PRIV_VALIDATOR_KEY_B64"), "no PRIV_VALIDATOR_KEY_B64 in .env to promote with"
    s = s.replace(key_anchor,
                  key_anchor + f"      - PRIV_VALIDATOR_KEY_B64={env['PRIV_VALIDATOR_KEY_B64']}\n")

# cloudflared: the tunnel token replaces the empty env list
anchor = "    env: []\n"
assert s.count(anchor) == 1, "cloudflared env anchor moved"
use_placeholder = fullnode and not want_tunnel
s = s.replace(anchor, "    env:\n      - TUNNEL_TOKEN=%s\n"
              % (TUNNEL_PLACEHOLDER if use_placeholder else env["TUNNEL_TOKEN"]))

# relayer: its key, only when the service is actually on. Injected the same way
# and for the same reason as the other two — it reaches the provider either way,
# what this avoids is it reaching a public repository.
# Skipped for --fullnode: that node runs the relayer disabled, so the key would
# be a secret shipped to a second provider for nothing. It pays gas and holds
# nothing else, but "low value" is not a reason to spread it.
anchor = "      - LINK_ON_START="
if anchor in s and not fullnode:
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
    for var in ("VALIDATOR_MNEMONIC", "NODE_KEY_B64"):
        assert var not in n, (
            f"--fullnode built an SDL carrying {var}. This node must hold no "
            "signing identity; refusing to submit one that does.")
    if want_valkey:
        assert n.get("PRIV_VALIDATOR_KEY_B64"), "--validator-key injected nothing"
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
            "--fullnode with no STATESYNC_RPC_SERVERS: earth-1 has applied "
            "consensus-breaking upgrades, so it cannot be replayed from genesis by "
            "one binary. Without state sync this node can never catch up. Pass "
            "--no-statesync if this node already holds the data, or if you are "
            "driving the binaries by hand.")
    else:
        # The assertion above is right about ONE binary and wrong about three,
        # and it is also wrong about a node that already has the blocks.
        #
        # Two nodes legitimately run without state sync. One RESUMES a volume it
        # already filled — it has the history and needs no catch-up mechanism at
        # all. The other REPLAYS earth-1 from block 1, crossing two upgrade
        # heights — 30100 (v0.6.0) and 50100 (v0.7.0) — at which the app halts on
        # purpose and refuses to continue on the old binary. Normally cosmovisor
        # swaps them; USE_COSMOVISOR=false plus a deploy.sh PUT per halt does the
        # same job without cosmovisor's height detection, which loses a race on a
        # small node (see the v0.6.0 halt postmortem) and is still RPC-only as of
        # v1.7.3 — verified in its scanner.go, not assumed.
        #
        # This is the ONLY thing --no-statesync relaxes. Every guarantee that
        # makes --fullnode safe — no mnemonic, no node key, no consensus key
        # without an explicit --validator-key — is asserted above and still
        # applies, because a node replaying history must no more double-sign
        # than one catching up by state sync.
        assert not n.get("STATESYNC_RPC_SERVERS"), (
            "--no-statesync with STATESYNC_RPC_SERVERS set: state sync would "
            "jump this node to a trusted height and it would never hold the "
            "early blocks that are the whole point of replaying.")
    # Present, but allowed to be empty. A node that has to reach the network
    # needs peers, and forgetting them is the mistake this catches. But
    # earth-1 currently runs ONE node: the archive and sync leases were closed
    # on 2026-09-01, so there is nobody to dial, and naming a dead peer only
    # produces a reconnect error every few seconds. An explicitly empty value
    # says "deliberately alone"; a missing key still says "you forgot".
    assert "PERSISTENT_PEERS" in n, (
        "--fullnode with no PERSISTENT_PEERS key: set it, empty if this node is "
        "deliberately the only one on the network")
    assert "RELAYER_MNEMONIC" not in r, (
        "--fullnode built an SDL carrying RELAYER_MNEMONIC; the relayer is off "
        "on this node and the key has no business reaching a second provider")
elif n.get("DEV_INIT") == "1":
    pass
else:
    assert n.get("PRIV_VALIDATOR_KEY_B64"), (
        "DEV_INIT=0 with no PRIV_VALIDATOR_KEY_B64 — this node would join with a "
        "random consensus key, hold no voting power, and the chain would have no "
        "signer. Set it in .env, or set DEV_INIT=1 for a throwaway chain.")
    try:
        k = json.loads(base64.b64decode(n["PRIV_VALIDATOR_KEY_B64"]))
    except Exception as e:
        raise AssertionError("PRIV_VALIDATOR_KEY_B64 is not base64 of JSON: %s" % e)
    assert "priv_key" in k and "address" in k, "PRIV_VALIDATOR_KEY_B64 is not a priv_validator_key.json"

assert n.get("CHAIN_ID") == "earth-1"
mn = n.get("VALIDATOR_MNEMONIC", "")
if not fullnode:
    assert len(mn.split()) in (12, 24), "validator mnemonic missing/malformed"
    assert not any(c in mn for c in "\"'"), "mnemonic carries quote characters; BIP39 will reject it"
    assert mn == mn.strip(), "mnemonic has leading/trailing whitespace"
    assert c.get("TUNNEL_TOKEN", "").startswith("eyJ"), "tunnel token missing or not a JWT"
else:
    if want_tunnel:
        assert c.get("TUNNEL_TOKEN", "").startswith("eyJ"), "--tunnel injected no real token"
    else:
        assert c.get("TUNNEL_TOKEN") == TUNNEL_PLACEHOLDER, (
            "--fullnode must not carry the live tunnel token: a second replica on "
            "that tunnel splits real traffic onto a node that is still syncing")
if r.get("ENABLED") == "true":
    rm = r.get("RELAYER_MNEMONIC", "")
    assert len(rm.split()) in (12, 24), "relayer enabled but its mnemonic is missing/malformed"
    assert not any(c in rm for c in "\"'"), "relayer mnemonic carries quote characters"
    for k in ("COUNTERPARTY_CHAIN_ID", "COUNTERPARTY_RPC", "COUNTERPARTY_PREFIX"):
        assert r.get(k), f"relayer enabled but {k} is unset"

img = svcs["node"]["image"]
assert img == svcs["relayer"]["image"], "node and relayer images differ"
print("services:   ", ", ".join(sorted(svcs)))
print("node image: ", img)
print("DEV_INIT:   ", n["DEV_INIT"], " CHAIN_ID:", n["CHAIN_ID"], " MIN_GAS:", n.get("MIN_GAS_PRICES"))
if not fullnode and n.get("DEV_INIT") != "1":
    kd = json.loads(base64.b64decode(n["PRIV_VALIDATOR_KEY_B64"]))
    print("consensus:   %s (from PRIV_VALIDATOR_KEY_B64)" % kd["address"])
    print("node key:    %s" % ("injected" if n.get("NODE_KEY_B64") else "MISSING - node id will change on reset"))
    print("reset:       RESET_ON_GENESIS_MISMATCH=%s" % n.get("RESET_ON_GENESIS_MISMATCH", "0"))
    print("external:    %s" % n.get("EXTERNAL_ADDRESS", "UNSET - peers cannot dial this node"))
if fullnode:
    print("MODE:        --fullnode%s%s%s" % (
        " +NO-STATESYNC" if no_statesync else "",
        " +VALIDATOR-KEY (THE SWAP)" if want_valkey else " — no consensus key",
        " +TUNNEL" if want_tunnel else " — tunnel placeholder"))
    if no_statesync:
        # Which of the two it is depends on the volume, which this script cannot
        # see, so say what was configured rather than guessing at the outcome.
        print("no statesync: resumes its volume, or replays from block 1 if empty"
              " — cosmovisor=%s" % n.get("USE_COSMOVISOR", "false"))
    else:
        print("state sync:  height %s via %s" % (n["STATESYNC_TRUST_HEIGHT"], n["STATESYNC_RPC_SERVERS"]))
    print("peers:       %s" % (n["PERSISTENT_PEERS"] or "(none — solo node)"))
else:
    print("secrets:     VALIDATOR_MNEMONIC(%d words), TUNNEL_TOKEN(%d chars)"
          % (len(n["VALIDATOR_MNEMONIC"].split()), len(c["TUNNEL_TOKEN"])))
print("relayer:     ENABLED=%s%s" % (r["ENABLED"],
      "  -> " + r.get("COUNTERPARTY_CHAIN_ID", "") + "  link=" + r.get("LINK_ON_START", "false")
      if r["ENABLED"] == "true" else ""))
print("wrote", out, "(%d bytes)" % len(s))
