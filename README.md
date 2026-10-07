# earth-network-deploy

Operator deployment for the earth chain. Private, because it holds the Akash SDL, the
secrets layout, and the runbooks for a node that has a validator key and a tunnel
token behind it.

The chain itself is public at `zenopie/earth-network-chain`: node software, genesis,
and a generic image with a minimal entrypoint that lets anyone run a node. Nothing
here is needed to *join* the network; it is only needed to operate this deployment.
That includes our node image (`node/`: key injection, the keyless guard, refusing a
foreign genesis, cosmovisor, the relayer), built `FROM` the chain image by digest,
the request filter in front of the public RPC and LCD (`edge/`), and earth-1's launch
identities (`launch/launch.json`, the input to the chain's ceremony script): this
operator's infrastructure and decisions, not node software, so they live here.

    RELAUNCH.md             the runbook for launching earth-1 from a fresh genesis
    akash/deploy.yaml       the deployed unit (node, edge filter, cloudflared, relayer)
    node/                   the node image: Dockerfile FROM the chain image (base.pin),
                            entrypoint.sh, relayer.sh, drop-root.sh, entrypoint_test.sh
    launch/launch.json      earth-1's launch identities (operator, consensus pubkey,
                            accounts to remove, consensus keys never to reuse)
    edge/                   earth-edge, the request filter: its own Go module and image
    edge/conformance/       its checks against CometBFT, grpc-gateway and the chain's
                            protos, at the chain commit in chain.pin (fetch-chain.sh)
    akash/README.md         how the lease behaves, and what destroys it
    akash/REMOTE_SIGNER.md  moving the consensus key behind tmkms
    akash/genesis.sha256    the genesis we mean to run (pinned by hand)
    bin/check-genesis.sh    a release's / checkout's genesis vs that pin
    bin/digest.sh           resolve a released tag to its image digest
    bin/ceremony.sh         run the chain repo's launch ceremony with launch/launch.json
    bin/build-node.sh       node/base.pin from a chain tag (--base); build + push the
                            node image (--pin: write its digest on node and relayer)
    bin/build-edge.sh       build + push the edge image, print (--pin: write) its digest
    bin/build-sdl.py        SDL + release check + secrets -> the copy that gets sent
    bin/create.sh           a NEW lease (new volume: the chain starts at height 1)
    bin/deploy.sh           update the lease in place (keeps the volumes)
    bin/lease-logs.py       container logs and kubernetes events, via the provider
    bin/lease-shell.py      a command inside a running container
    bin/check-edge.py       from outside: rpc/lcd reach the edge filter, not the node

## Deploying

    FLAGS="--fullnode --no-statesync --validator-key --node-key --tunnel"
    bin/deploy.sh <tag> $FLAGS            update the lease in place
    bin/deploy.sh <tag> $FLAGS --print    build the SDL and read it, send nothing

The flags are the validator's, the same ones `create.sh` made the lease with
(RELAUNCH.md, section 3). Leaving one out is refused or drops a secret the node needs.

In place keeps the volumes, so the chain keeps its height and history. Image and env
changes can go this way. Endpoint kinds and resources cannot, because they are part of
what the provider bid on: those need a close-and-recreate, which **destroys the
chain's state**. A pod that is not Ready (crash-looping, or sleeping until
`genesis_time`) is never replaced by a PUT, though the PUT is accepted.

## One node

earth-1 runs one node: the validator, which is also the full-history node behind
rpc.erth.network and lcd.erth.network, and the RPC the privacy indexer reads
`block_results` from. The public reaches its RPC and LCD only through `edge`, the
request filter in the same lease (akash/README.md, "Public RPC and LCD"). It never prunes and never state syncs, so its 200Gi data volume
only grows. Akash cannot grow a volume in place: a bigger one is a new lease and a
replay from block 1. Watch it, and alert well before half full:

    bin/lease-shell.py --service node -- df -h /data

See RELAUNCH.md, section 3 ("Disk").

## Which digests are committed

The chain repo used to rewrite this SDL with the image digest and commit it back to
master. That put deployment state in a public repository, and a build racing a human
push meant one of them lost. The chain repo now knows nothing of this deployment.

Both images the lease runs are built here, so their digests are committed in
`akash/deploy.yaml`: the node's (and relayer's) by `bin/build-node.sh --pin`
(RELAUNCH.md 1.5), with the chain image it was built `FROM`, and the edge's by
`bin/build-edge.sh --pin` (1.4). The chain release is still resolved at deploy
time: `bin/digest.sh <tag>` returns the chain image's `ghcr.io/...@sha256:...`
with no credentials, and `build-sdl.py` refuses unless it is `node/base.pin`, the
base of the committed node image.

    node/entrypoint_test.sh                                           # the node entrypoint
    (cd edge && go test ./...)                                        # the filter
    edge/conformance/fetch-chain.sh && (cd edge/conformance && go test ./...)

## Secrets

`.env`, gitignored, shape in `.env.example`. Secrets are injected into the submitted
copy of the SDL, never into the file on disk. The validator is built with
`--fullnode` (RELAUNCH.md, section 3), which sends only what a flag asks for:
`--validator-key` (`PRIV_VALIDATOR_KEY_B64`), `--node-key` (`NODE_KEY_B64`) and
`--tunnel` (`TUNNEL_TOKEN`). `VALIDATOR_MNEMONIC` and `RELAYER_MNEMONIC` then never
reach the lease: the operator account signs from the operator's machine. Without
`--fullnode`, `build-sdl.py` refuses this SDL: the devnet anchor it injected the
mnemonic after is gone on purpose.

Everything submitted reaches the provider regardless; that is what submitting means.
What the injection avoids is the secrets reaching a repository.

The `AKASH_API_KEY` is the dangerous one: it can close the lease, and closing destroys
the volumes that hold the chain.
