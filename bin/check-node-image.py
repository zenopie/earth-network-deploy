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

Labels are the build's own claim: an image built elsewhere could carry them on
any base (round-8 R8-D-2). So the base is also checked from content: the chain
image's manifest and config are fetched by its digest (for an index, its
linux/amd64 image) and verified the same way, and its layers (the config's
rootfs.diff_ids, the sha256 of each uncompressed layer) must be the first
layers of the node image, in order. The node image is then the base's
filesystem with layers on top. What those layers add is node/Dockerfile's,
which build-node.sh requires to be committed; this does not re-check them.

Exit 0 when both labels match and the base's layers are the node image's first
layers, 1 otherwise. bin/deploy.sh and bin/create.sh run
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
INDEX_TYPES = ", ".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
])
DIGEST = r"sha256:[0-9a-f]{64}"


def fail(msg):
    print("check-node-image: " + msg, file=sys.stderr)
    sys.exit(1)


def get(url, token, accept=None):
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    if accept:
        req.add_header("Accept", accept)
    with urllib.request.urlopen(req, timeout=60) as r:  # follows the blob redirect
        return r.read()


def token(repo):
    return json.loads(urllib.request.urlopen(
        "https://ghcr.io/token?scope=repository:%s:pull&service=ghcr.io" % repo, timeout=60).read())["token"]


def fetch_verified(repo, tok, digest, accept=None):
    """The blob or manifest at digest, refused unless it hashes to digest."""
    kind = "manifests" if accept else "blobs"
    body = get("https://ghcr.io/v2/%s/%s/%s" % (repo, kind, digest), tok, accept)
    if "sha256:" + hashlib.sha256(body).hexdigest() != digest:
        fail("the registry's %s %s of %s does not hash to it" % (kind[:-1], digest, repo))
    return body


def image(repo, digest, allow_index):
    """(config dict, manifest layer count) of the image at digest, every byte
    verified by digest. An index is followed to its linux/amd64 image only
    when allow_index (the chain image may be multi-platform; the node image
    is one image, as build-node.sh pushes it)."""
    tok = token(repo)
    accept = MANIFEST_TYPES + (", " + INDEX_TYPES if allow_index else "")
    mj = json.loads(fetch_verified(repo, tok, digest, accept))
    if "manifests" in mj:
        if not allow_index:
            fail("%s is an index, not a single-image manifest (build-node.sh pushes one image)" % digest)
        amd = [m for m in mj["manifests"]
               if (m.get("platform") or {}).get("os") == "linux"
               and (m.get("platform") or {}).get("architecture") == "amd64"]
        if len(amd) != 1 or not re.fullmatch(DIGEST, amd[0].get("digest", "")):
            fail("index %s of %s has %d linux/amd64 images, not one" % (digest, repo, len(amd)))
        return image_at(repo, tok, amd[0]["digest"])
    return image_from(repo, tok, digest, mj)


def image_at(repo, tok, digest):
    mj = json.loads(fetch_verified(repo, tok, digest, MANIFEST_TYPES))
    if "manifests" in mj:
        fail("%s of %s: an index inside an index" % (digest, repo))
    return image_from(repo, tok, digest, mj)


def image_from(repo, tok, digest, mj):
    cfg_digest = (mj.get("config") or {}).get("digest", "")
    if not re.fullmatch(DIGEST, cfg_digest):
        fail("%s of %s is not a single-image manifest" % (digest, repo))
    cfg = json.loads(fetch_verified(repo, tok, cfg_digest))
    diff_ids = (cfg.get("rootfs") or {}).get("diff_ids")
    layers = mj.get("layers")
    if not isinstance(diff_ids, list) or not isinstance(layers, list) or len(diff_ids) != len(layers) \
            or not all(isinstance(d, str) and re.fullmatch(DIGEST, d) for d in diff_ids):
        fail("%s of %s: its config's rootfs.diff_ids do not match its %s manifest layers"
             % (digest, repo, len(layers) if isinstance(layers, list) else "?"))
    return cfg, len(layers)


def main(argv):
    if len(argv) != 4:
        fail("usage: check-node-image.py <node image @sha256> <chain image @sha256> <genesis sha256>")
    img, base, gsha = argv[1:]
    m = re.fullmatch(r"ghcr\.io/" + re.escape(REPO) + r"@(" + DIGEST + ")", img)
    if not m:
        fail("node image %r is not ghcr.io/%s@sha256:<64 hex>" % (img, REPO))
    digest = m.group(1)
    bm = re.fullmatch(r"ghcr\.io/" + re.escape(CHAIN_REPO) + r"@(" + DIGEST + ")", base)
    if not bm:
        fail("base %r is not ghcr.io/%s@sha256:<64 hex>" % (base, CHAIN_REPO))
    if not re.fullmatch(r"[0-9a-f]{64}", gsha):
        fail("genesis sha256 %r is not 64 hex" % gsha)

    cfg, _ = image(REPO, digest, allow_index=False)
    labels = (cfg.get("config") or {}).get("Labels") or {}
    got_base = labels.get("network.erth.base")
    got_gsha = labels.get("network.erth.genesis-sha256")
    bad = []
    if got_base != base:
        bad.append("built FROM %r, expected %s (node/base.pin)" % (got_base, base))
    if got_gsha != gsha:
        bad.append("base genesis %r, expected %s (akash/genesis.sha256)" % (got_gsha, gsha))

    # The base from content, not from the label (R8-D-2).
    base_cfg, _ = image(CHAIN_REPO, bm.group(1), allow_index=True)
    base_ids = base_cfg["rootfs"]["diff_ids"]
    node_ids = cfg["rootfs"]["diff_ids"]
    if not base_ids:
        bad.append("the base image has no layers")
    elif node_ids[:len(base_ids)] != base_ids:
        same = 0
        while same < min(len(base_ids), len(node_ids)) and base_ids[same] == node_ids[same]:
            same += 1
        bad.append("its layers do not start with the base's: %d of the base's %d layers match "
                   "(it was not built FROM %s)" % (same, len(base_ids), base.split("@")[-1][:19]))
    if bad:
        fail("%s: %s. Rebuild it: bin/build-node.sh --pin (RELAUNCH.md 1.5)" % (img[:72], "; ".join(bad)))
    print("node image:   built FROM %s… (labels, and its first %d layers are the base's), genesis %s…"
          % (base.split("@")[-1][:19], len(base_ids), gsha[:12]))


if __name__ == "__main__":
    try:
        main(sys.argv)
    except (urllib.error.URLError, ValueError, KeyError, TypeError, AttributeError) as e:
        fail("cannot read %s from the registry anonymously (%s): is the image pushed and its "
             "package public?" % (sys.argv[1] if len(sys.argv) > 1 else "the image", e))
