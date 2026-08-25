# Akash deployment — earth chain node

Two services in one deployment: the validator and a Cloudflare Tunnel connector.

    node          earthd, the single validator   -> /data
    cloudflared   Cloudflare Tunnel connector

The ads-for-gas service is a separate repo, image and lease
(`earth-network-backend`), with its own tunnel. Closing a lease destroys its
volumes, and this one holds the chain's state — a backend change must not be
able to take it.

    deploy/akash/deploy.yaml       the deployed unit
    docker/entrypoint.sh    first-boot genesis, then earthd start

## Build the image first

CI builds and pushes on `v[0-9]+.[0-9]+.[0-9]+` tags only — a push to `master`
builds nothing — and then rewrites both `image:` lines in `deploy.yaml` with the
digest it just pushed and commits that back to master.

    git tag v0.1.0 && git push origin v0.1.0
    git pull origin master     # picks up the pinned digest

So after a release, pull and deploy: the SDL already points at the bytes CI
built. The package must be public on ghcr.io, or the provider cannot pull it.

## Deploy

Secrets are NOT in the SDL. Two values are injected into the submitted copy from
the gitignored `.env` at the repo root:

    TUNNEL_TOKEN          anyone holding it can attach a replica to your tunnel

It still reaches the provider — everything in a submitted SDL does. What the
injection avoids is them reaching a public repository.

    POST /v1/deployments   {"data": {"sdl": "<sdl>", "deposit": 5}}
    GET  /v1/bids/{dseq}
    POST /v1/leases        {"manifest": "<from create>", "leases": [...]}

Auth is `x-api-key` (Console API, managed wallet / credit card). There is no
`provider-services login` — that CLI signs with a local key and has no session.

## One lease holds both volumes

Closing it destroys the chain's state — genesis, the validator's consensus key,
every account — and the replay database with it. The next deploy is not a
restart; it is a different chain.

Image and env changes go in place with `PUT /v1/deployments/{dseq}` and keep the
volumes. Structural changes do not: endpoint kinds and resources are part of what
the provider bid on, and the API rejects them with `over-utilized PORT
endpoints`. Those need a close-and-recreate.

If you are on an Akash trial, deployments auto-close after 24 hours, which is the
same thing.

## IBC relayer

A third service, off by default (`ENABLED=false`). It shares the node's image —
one image is one digest for CI to pin — and runs `docker/relayer.sh`
instead of the node entrypoint.

Co-locating it with the validator is safe in a way nothing else here is. A
relayer cannot forge packets or move funds; it only submits proofs that both
chains verify for themselves. Its key pays gas and holds nothing else, so the
failure mode is delayed packets, not theft.

To turn it on, set `ENABLED=true` and supply the counterparty:

    - ENABLED=true
    - COUNTERPARTY_CHAIN_ID=cosmoshub-4
    - COUNTERPARTY_RPC=https://...
    - COUNTERPARTY_PREFIX=cosmos
    - COUNTERPARTY_GAS_PRICES=0.025uatom

`RELAYER_MNEMONIC` is injected at deploy time from `.env`, never committed. Set
`LINK_ON_START=true` for one deploy to create the client, connection and channel,
then put it back — linking spends gas on both chains and is not something a
restart should retry.

The config is written once into `/data/relayer` and reused, so changing the env
afterwards does nothing until that directory is cleared.

**It needs funding on BOTH chains.** Earth is no longer zero-fee — the node sets
`MIN_GAS_PRICES=0.005uerth` to stop free spam — so the relayer pays uerth to
deliver a packet here, and the counterparty's token to deliver one there. A
relayer whose balance runs dry on either side stops relaying silently.

This note used to say earth was free and only the counterparty needed funding.
That was true when it was written and stopped being true when the minimum was
raised; `relayer.sh` still had `"gas-prices":"0uerth"` to match, which would
have had every packet rejected for insufficient fees on first use.

`earth-ibc-test` in the projects folder is the local two-chain rig this was
derived from.

### Verified against osmo-test-5, 2026-08-25

    earth channel-0  <->  osmo-test-5 channel-11830   transfer / ics20-1
    1 ERTH delivered as ibc/7035253F26470779EFAF2941FFDCCBE7763EEDBB87983E2F51CCEDA3611E81FD
    cost to establish: ~7,000 uerth + ~79,000 uosmo

Three things had to be fixed to get there, none of which had ever run:

- **earth's own gas price was 0uerth** while the node demands 0.005. See above.
- **rly cannot read a chain that hosts 08-wasm light clients.** Osmosis does —
  that is how Celestia and Ethereum reach it — and rly v2.6.0 has no such type
  registered, so its scan for a reusable client dies before any transaction is
  sent. `--override` skips the scan. rly's last release is from January 2025 and
  builds against ibc-go v8.2.0; this chain runs v10.5.0.
- **Osmosis prices fees with a moving EIP-1559 base fee**, enforced chain-level
  by x/txfees. `/cosmos/base/node/v1beta1/config` reports an empty minimum and
  tells you nothing. Read `/osmosis/txfees/v1beta1/cur_eip_base_fee` and set
  well clear of it.

All three failed the same way: the relayer looked healthy, the counterparty
balance never moved, and only its logs said why. If a link stalls, read the
relayer's logs first — everything checkable from outside will look fine.

**Client expiry.** A light client dies if it is not updated within its trusting
period, and an expired client cannot be revived by a newer header — it needs
governance substitution or a new channel. The link uses 66% of the
counterparty's unbonding period, the IBC convention and Hermes' default, rather
than rly's 85%. Against Osmosis mainnet that is ~9 days; against osmo-test-5,
whose unbonding is a testnet-shortened 5 days, it is ~3. A relayer down over a
long weekend loses a testnet channel.

## Addresses

The node is reachable **only through the tunnel** — `lcd.erth.network` and
`rpc.erth.network`. Neither 1317 nor 26657 is published on a provider port, so
there is no address to update when Akash reassigns external ports on a new
lease, which is what the tunnel is for.

One port is published, and it is not the application: `cloudflared`'s metrics on
2000. Akash rejects a manifest with `zero global services`, so something has to
be. Publishing the connector rather than the node keeps the chain private and
gives `/ready` as a health check — worth having, because the Console API exposes
no logs endpoint and a connector that fails to start is otherwise completely
silent. Read its external port from the lease status:

    curl http://<provider>:<port>/ready
    {"status":200,"readyConnections":4,...}

Nothing is exposed `as: 80`. That asks the provider for an HTTP ingress on a
generated hostname; on the first lease of this deployment the pod went ready and
served RPC while the hostname returned nginx 404 for ten minutes,
indistinguishable from a hostname that was never registered. Mapped ports came
up immediately, and the tunnel replaced them once its hostnames were verified.

Ordering matters here and is not reversible: endpoint kinds are part of what a
provider bids on, so the global ports could not be removed in place. They stayed
until lcd.* and rpc.* were confirmed serving, and taking them out required a
close-and-recreate, which destroyed the volume.

**One tunnel per deployment, not per service.** A tunnel's replicas are chosen
by proximity with no traffic steering, so connectors able to reach different
origins would black-hole requests non-deterministically. This tunnel serves this
deployment; the backend has its own. Configure the Public Hostnames on
Cloudflare's side:

    lcd.* -> http://node:1317      rpc.* -> http://node:26657

The backend must NOT be leased on the same provider as this node. Its
`EARTH_NODE_URL` is a provider hostname and NodePort, and from inside that same
provider's cluster it is a hairpin back to itself which hangs rather than
failing — cosmpy builds its LedgerClient inside FastAPI's startup event, so
uvicorn never finishes starting, the port completes a TCP handshake and then
never answers, and the lease still reports ready because nothing defines a
readiness probe. Pointing it at this tunnel's `lcd.*` hostname avoids the
question entirely.

## Devnet posture

Both of these were unconditional and are now opt-in, off by default:

- `API_UNSAFE_CORS` — any origin may read the LCD *and broadcast through it*.
- `DEV_INIT` — makes a new chain on every fresh volume rather than joining one.

`MIN_GAS_PRICES` is `0.005uerth`, not zero. Free transactions are free spam, and
the gas burn that makes fees deflationary collects nothing to burn at zero.

Genesis no longer funds any devnet account. The ads-for-gas hot wallet is out —
its key had been on a laptop, and a launch genesis is not the place to fund an
operational wallet from one. **It therefore has no funds at height 1**; fund it
after launch from the validator, with a key that has never left a signer. Only
the genesis validator and the dex module hold anything.
