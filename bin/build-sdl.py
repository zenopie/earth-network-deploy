#!/usr/bin/env python3
"""Build the SDL that actually gets submitted: akash/deploy.yaml + the image
digest for a released tag + secrets from .env.

Textual insertion, not a YAML round-trip, so the committed file's comments and
exact shape survive into what the provider receives. A round-trip would also
risk turning version: "2.0" into a float, which the SDL parser rejects.
"""
import base64
import json
import os, re, sys, yaml

repo   = sys.argv[1]           # this repo
out    = sys.argv[2]           # where to write the submitted copy
digest = sys.argv[3] if len(sys.argv) > 3 else None   # ghcr.io/...@sha256:...

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

s = open(os.path.join(repo, "akash/deploy.yaml")).read()

# Pin the image at deploy time rather than at build time. The chain repo used to
# rewrite this and commit it back; it no longer knows this file exists.
if digest:
    s, n = re.subn(r'(?m)^(\s*image:\s*)ghcr\.io/\S+', r'\1' + digest, s)
    assert n, "no ghcr image line to pin"
    print("pinned %d image line(s) to %s" % (n, digest.split("@")[-1][:19] + "…"))

# node: the devnet validator key, appended after the last VALIDATOR_* line
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
for var in ("PRIV_VALIDATOR_KEY_B64", "NODE_KEY_B64"):
    if env.get(var):
        s = s.replace(anchor, anchor + f"      - {var}={env[var]}\n")

# cloudflared: the tunnel token replaces the empty env list
anchor = "    env: []\n"
assert s.count(anchor) == 1, "cloudflared env anchor moved"
s = s.replace(anchor, f"    env:\n      - TUNNEL_TOKEN={env['TUNNEL_TOKEN']}\n")

# relayer: its key, only when the service is actually on. Injected the same way
# and for the same reason as the other two — it reaches the provider either way,
# what this avoids is it reaching a public repository.
anchor = "      - LINK_ON_START="
if anchor in s:
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
if n.get("DEV_INIT") == "1":
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
assert len(mn.split()) in (12, 24), "validator mnemonic missing/malformed"
assert not any(c in mn for c in "\"'"), "mnemonic carries quote characters; BIP39 will reject it"
assert mn == mn.strip(), "mnemonic has leading/trailing whitespace"
assert c.get("TUNNEL_TOKEN", "").startswith("eyJ"), "tunnel token missing or not a JWT"
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
if n.get("DEV_INIT") != "1":
    kd = json.loads(base64.b64decode(n["PRIV_VALIDATOR_KEY_B64"]))
    print("consensus:   %s (from PRIV_VALIDATOR_KEY_B64)" % kd["address"])
    print("node key:    %s" % ("injected" if n.get("NODE_KEY_B64") else "MISSING - node id will change on reset"))
    print("reset:       RESET_ON_GENESIS_MISMATCH=%s" % n.get("RESET_ON_GENESIS_MISMATCH", "0"))
    print("external:    %s" % n.get("EXTERNAL_ADDRESS", "UNSET - peers cannot dial this node"))
print("secrets:     VALIDATOR_MNEMONIC(%d words), TUNNEL_TOKEN(%d chars)"
      % (len(n["VALIDATOR_MNEMONIC"].split()), len(c["TUNNEL_TOKEN"])))
print("relayer:     ENABLED=%s%s" % (r["ENABLED"],
      "  -> " + r.get("COUNTERPARTY_CHAIN_ID", "") + "  link=" + r.get("LINK_ON_START", "false")
      if r["ENABLED"] == "true" else ""))
print("wrote", out, "(%d bytes)" % len(s))
