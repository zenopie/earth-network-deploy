# The validator's node image: the chain's generic image, pinned by digest,
# plus what only our deployment needs.
#
#   bin/build-node.sh     builds this for linux/amd64 from node/base.pin,
#                         pushes ghcr.io/zenopie/earth-network-node and pins
#                         its digest in akash/deploy.yaml (RELAUNCH.md 1.5)
#
# From the chain image it keeps earthd, libwasmvm, the verifier's libc++
# runtime, the release genesis and its sha256 (/etc/earth/), and the `earth`
# user. It replaces the chain's minimal entrypoint with node/entrypoint.sh
# (keys from the environment, the keyless guard, refusing a foreign genesis,
# cosmovisor) and adds cosmovisor and the IBC relayer, which the chain image
# does not carry.
#
# CHAIN_IMAGE has no default: the build passes node/base.pin, so the base is
# always a digest this repository names, never a tag.
ARG CHAIN_IMAGE

# ---- tools ----------------------------------------------------------------
# Built here rather than in the chain image: they are this deployment's
# choices. Same pinned Go as edge/Dockerfile. Built for the target platform
# (no $BUILDPLATFORM): rly needs cgo against the same glibc the runtime has.
FROM golang:1.25.10-trixie@sha256:adc12eea75e38959793d88b5c15f9f8ad20fee5ece9d3d5ae05c7d7c2d430f4b AS tools
ENV GOTOOLCHAIN=local

# cosmovisor supervises earthd across upgrades: at an upgrade height the chain
# halts on purpose, and cosmovisor swaps in the binary the governance plan
# names (checksum required) and restarts. Pure Go, so CGO off gives a static
# binary. Pinned: it decides which binary runs the chain.
#
# The module proxy and checksum database fail intermittently (a stream
# INTERNAL_ERROR mid-download); retry rather than weaken verification. The
# trailing test is load-bearing: a bare loop exits 0 even when every attempt
# failed.
RUN for i in 1 2 3; do CGO_ENABLED=0 GOBIN=/out go install cosmossdk.io/tools/cosmovisor/cmd/cosmovisor@v1.7.1 && ok=1 && break; echo "cosmovisor install failed (attempt $i)"; sleep 15; done; [ "${ok:-}" = 1 ]

# The IBC relayer, a second entry point in the same image (node/relayer.sh):
# one image, one digest. Installed from the module root, as `relayer`.
# CGO_ENABLED=1: with cgo off, go-ethereum compiles signature_nocgo.go, which
# calls btc_ecdsa.SignCompact with the wrong arity for the btcec version this
# dependency graph resolves to. The runtime is the chain image's Debian, so a
# dynamically linked relayer runs there.
RUN for i in 1 2 3; do CGO_ENABLED=1 GOBIN=/out go install github.com/cosmos/relayer/v2@v2.6.0 && ok=1 && break; echo "rly install failed (attempt $i)"; sleep 15; done; [ "${ok:-}" = 1 ] \
    && mv /out/relayer /out/rly

# ---- runtime ----------------------------------------------------------------
FROM ${CHAIN_IMAGE}

# Root at start: node/drop-root.sh hands /data to `earth` (a fresh provider
# volume is root-owned) and re-executes as `earth`, so earthd and the relayer
# never run as root. setpriv is util-linux, in every Debian image; checked
# here so a base without it fails the build rather than every start.
USER root
RUN command -v setpriv >/dev/null && id earth >/dev/null \
    && command -v earthd >/dev/null \
    && [ -f /etc/earth/genesis.json ] && [ -f /etc/earth/genesis.json.sha256 ]

COPY --from=tools /out/cosmovisor /usr/local/bin/cosmovisor
COPY --from=tools /out/rly /usr/local/bin/rly
COPY entrypoint.sh relayer.sh drop-root.sh /usr/local/bin/
RUN chmod 755 /usr/local/bin/entrypoint.sh /usr/local/bin/relayer.sh \
    && chmod 644 /usr/local/bin/drop-root.sh \
    && (cosmovisor version >/dev/null 2>&1 || cosmovisor help >/dev/null) \
    && rly version >/dev/null

ENV EARTH_HOME=/data
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
