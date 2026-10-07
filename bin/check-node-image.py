#!/usr/bin/env python3
"""Check what a node image was really built from, by reading its labels from
the registry (anonymously, as the provider pulls it).

    bin/check-node-image.py <node image @sha256> <chain image @sha256> <genesis sha256>

node/Dockerfile writes two labels, from build arguments bin/build-node.sh
passes and the build itself checks:
    network.erth.base            the chain image it was built FROM (node/base.pin)
    network.erth.genesis-sha256  the sha256 of that base's baked genesis, which the
                                 build refuses unless it equals akash/genesis.sha256
The `# FROM` comment on the SDL's image lines is what build-sdl.py checks; this
checks the image behind the digest says the same (round-7 R7-D-6). The manifest
and config are fetched by digest and their sha256 verified, so the labels read
are the ones the provider will run.

Exit 0 when both labels match, 1 otherwise. bin/deploy.sh and bin/create.sh run
it on the SDL's node image before they send anything; bin/build-node.sh after
its push.
"""
import hashlib
import json
import os
import re
import sys
import urllib.error
import urllib.request

REPO = os.environ.get("NODE_IMAGE_REPO", "zenopie/earth-network-node")
CHAIN_REPO = "zenopie/earth-network-chain"
MANIFEST_TYPES = ", ".join([
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])


def fail(msg):
    print("check-node-image: " + msg, file=sys.stderr)
    sys.exit(1)


def get(url, token, accept=None):
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    if accept:
        req.add_header("Accept", accept)
    with urllib.request.urlopen(req, timeout=60) as r:  # follows the blob redirect
        return r.read()


def main(argv):
    if len(argv) != 4:
        fail("usage: check-node-image.py <node image @sha256> <chain image @sha256> <genesis sha256>")
    image, base, gsha = argv[1:]
    m = re.fullmatch(r"ghcr\.io/" + re.escape(REPO) + r"@(sha256:[0-9a-f]{64})", image)
    if not m:
        fail("node image %r is not ghcr.io/%s@sha256:<64 hex>" % (image, REPO))
    digest = m.group(1)
    if not re.fullmatch(r"ghcr\.io/" + re.escape(CHAIN_REPO) + r"@sha256:[0-9a-f]{64}", base):
        fail("base %r is not ghcr.io/%s@sha256:<64 hex>" % (base, CHAIN_REPO))
    if not re.fullmatch(r"[0-9a-f]{64}", gsha):
        fail("genesis sha256 %r is not 64 hex" % gsha)

    tok = json.loads(urllib.request.urlopen(
        "https://ghcr.io/token?scope=repository:%s:pull&service=ghcr.io" % REPO, timeout=60).read())["token"]
    man = get("https://ghcr.io/v2/%s/manifests/%s" % (REPO, digest), tok, MANIFEST_TYPES)
    if "sha256:" + hashlib.sha256(man).hexdigest() != digest:
        fail("the registry's manifest for %s does not hash to it" % digest)
    mj = json.loads(man)
    cfg_digest = (mj.get("config") or {}).get("digest", "")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", cfg_digest):
        fail("%s is not a single-image manifest (an index? build-node.sh pushes one image)" % digest)
    cfg = get("https://ghcr.io/v2/%s/blobs/%s" % (REPO, cfg_digest), tok)
    if "sha256:" + hashlib.sha256(cfg).hexdigest() != cfg_digest:
        fail("the image config does not hash to %s" % cfg_digest)
    labels = (json.loads(cfg).get("config") or {}).get("Labels") or {}

    got_base = labels.get("network.erth.base")
    got_gsha = labels.get("network.erth.genesis-sha256")
    bad = []
    if got_base != base:
        bad.append("built FROM %r, expected %s (node/base.pin)" % (got_base, base))
    if got_gsha != gsha:
        bad.append("base genesis %r, expected %s (akash/genesis.sha256)" % (got_gsha, gsha))
    if bad:
        fail("%s: %s. Rebuild it: bin/build-node.sh --pin (RELAUNCH.md 1.5)" % (image[:72], "; ".join(bad)))
    print("node image:   built FROM %s…, genesis %s… (registry labels)" % (base.split("@")[-1][:19], gsha[:12]))


if __name__ == "__main__":
    try:
        main(sys.argv)
    except (urllib.error.URLError, ValueError, KeyError) as e:
        fail("cannot read %s from the registry anonymously (%s): is the image pushed and its "
             "package public?" % (sys.argv[1] if len(sys.argv) > 1 else "the image", e))
