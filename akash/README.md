# Akash deployment: the earth chain node

One deployment, three services:

    node          earthd, the validator and full-history RPC   -> /data
    cloudflared   Cloudflare Tunnel connector (lcd.*, rpc.*)
    relayer       IBC relayer, off by default (ENABLED=false)

The backend (gas grants and the privacy indexer) is a separate repo, image and lease
(`earth-network-backend`), with its own tunnel. Closing a lease destroys its volumes,
and this one holds the chain's state: a backend change must not be able to take it.
The image's entrypoint (`docker/entrypoint.sh` in the chain repo) installs the baked
genesis, checks its sha256, and starts `earthd` under cosmovisor.

## The image

CI in the chain repo builds and pushes on `v[0-9]+.[0-9]+.[0-9]+` tags only. The
digest is not committed here: `bin/digest.sh <tag>` reads it from the registry at
deploy time, and `bin/deploy.sh` / `bin/create.sh` pin the submitted copy to it. The
package must be public on ghcr.io, or the provider cannot pull it.

## Deploy

`bin/create.sh` makes a new lease (new volume); `bin/deploy.sh` updates one in place.
Both take the validator's flags (see the repo README and RELAUNCH.md). Secrets come
from the gitignored `.env` at the repo root and go only into the submitted copy:
`PRIV_VALIDATOR_KEY_B64`, `NODE_KEY_B64` and `TUNNEL_TOKEN`. They still reach the
provider, as everything in a submitted SDL does; what the injection avoids is a
repository.

The Console API, as these scripts use it:

    POST /v1/deployments         {"data": {"sdl": "<sdl>", "deposit": 5}}   -> 201
    GET  /v1/bids/{dseq}
    POST /v1/leases              {"manifest": ..., "leases": [...]}   # top level, not under data
    PUT  /v1/deployments/{dseq}  {"data": {"sdl": "<sdl>"}}           # in place

Auth is `x-api-key` (Console managed wallet). There is no `provider-services login`.
Logs and a shell are the provider's, with a JWT Console mints: `bin/lease-logs.py` and
`bin/lease-shell.py`.

## One lease holds the chain

Closing it destroys the chain's state: genesis, the validator's consensus state, every
account and all history. The next deploy is not a restart; it is a different chain.

Image and env changes go in place with PUT and keep the volumes. Structural changes do
not: endpoint kinds and resources are part of what the provider bid on, and the API
rejects them with `over-utilized PORT endpoints`. Those need a close-and-recreate.

A PUT replaces the pod only when it is Ready. A crash-looping pod, or one sleeping
until `genesis_time`, keeps running the old manifest: the PUT is accepted and
`updated_replicas` stays 0. Recovery then needs the provider to delete pod `node-0`.
After launch a new lease is not recovery: it is a new chain (RELAUNCH.md, section 9),
so a fix to a running chain goes in while the pod is Ready (RELAUNCH.md, section 8).

Akash trial deployments auto-close after 24 hours, which is the same thing.

## Node settings worth knowing

- `DEV_INIT=0`: the node installs the release genesis, verifies its sha256 and joins.
  `DEV_INIT=1` would make a throwaway chain with its own genesis_time and gentx on
  every fresh volume.
- `MIN_GAS_PRICES=0.005uerth`, ERTH only: ANML exists only in the shielded pool and is
  refused as a fee. Free transactions are free spam, and the fee burn collects nothing
  at zero.
- `EARTHD_MEMPOOL_MAX_TXS=-1`: private txs carry no signer, and every other app
  mempool rejects them.
- Full history: `EARTHD_PRUNING=nothing`, `EARTHD_MIN_RETAIN_BLOCKS=0`, ABCI responses
  kept, txs indexed, `UNSAFE_SKIP_BACKUP=true` (cosmovisor's pre-upgrade copy of
  `data/` is the likely cause of the full disk that ended the v0.9.2 chain).
- `API_UNSAFE_CORS=1` and `RPC_CORS_ORIGINS=https://erth.network`: the web app reads
  the LCD from the browser. The SDK's LCD CORS is all-or-nothing; while one node is
  both validator and public LCD, this is the trade.

## No logs

`NO_LOGS.md` at the repo root is the policy. Here: `EARTHD_LOG_LEVEL=*:info,rpc-server:error`
on the node and `--loglevel info` on cloudflared, both refused by `build-sdl.py` at
debug or trace. The Cloudflare settings it needs are listed there.

## Public RPC and LCD limits

`rpc.erth.network` and `lcd.erth.network` are served by the validator process, the
network's only signer. Two layers keep a query flood from slowing block production.

**On the node** (`akash/deploy.yaml`, refused by `build-sdl.py` when missing):
`EARTHD_QUERY_GAS_LIMIT=50000000` (one gRPC/LCD/abci_query query; the SDK default is
unbounded), `EARTHD_RPC_MAX_OPEN_CONNECTIONS=100`,
`EARTHD_RPC_MAX_SUBSCRIPTION_CLIENTS=20`, `EARTHD_API_MAX_OPEN_CONNECTIONS=200`,
`EARTHD_RPC_UNSAFE=false`. These are env, so changing one is an in-place PUT.

**At Cloudflare** (zone `erth.network`, Security → WAF), set before launch. What
the clients use decides the rules: the backend indexer reads `/status`,
`/blockchain`, `/block_results` and `/abci_query` (never with `prove`); the apps'
explorer reads `/cosmos/tx/v1beta1/txs?query=…`; nothing calls `tx_search`,
`block_search` or subscribes.

1. Custom rule, **Block**: host `rpc.erth.network` and URI path in `/tx_search`,
   `/block_search`, `/unconfirmed_txs`, `/dial_seeds`, `/dial_peers`, or a path
   starting `/unsafe`; or the query string contains `prove=true`. On a plan with
   request-body fields, also block a POST whose body contains `"tx_search"`,
   `"block_search"` or `"prove":true` (CosmJS sends JSON-RPC as POST to `/`).
2. Rate limit, per IP: host `lcd.erth.network` and path starting
   `/cosmos/tx/v1beta1/txs` with method GET (tx search; with `tx.height>0` and
   `ORDER_BY_DESC` it scans the whole index): 20 requests per minute, then block for
   10 minutes.
3. Rate limit, per IP: host `lcd.erth.network` or `rpc.erth.network`, everything:
   600 requests per minute, then block for 1 minute. A re-index from height 1 runs
   faster than that: add a skip rule for the backend lease's egress IP while it
   catches up, and remove it after.
4. No caching rule: heights move, and a cached `/status` would stall the indexer.

Check from outside: `curl -s 'https://rpc.erth.network/tx_search?query="tx.height=1"'`
is blocked (403), and `/status` still answers.

## Addresses

The node is reachable for clients **only through the tunnel**: `lcd.erth.network` and
`rpc.erth.network`. Neither 1317 nor 26657 is on a provider port, so a new lease
changes no client address. Configure the Public Hostnames on Cloudflare's side:

    lcd.* -> http://node:1317      rpc.* -> http://node:26657

P2P (26656) is the one application port on a provider port, because CometBFT's
protocol is not HTTP. The provider assigns it, so after a new lease set
`EXTERNAL_ADDRESS` to it (in place, once the pod is Ready). A stale value only stops
inbound peers; the node still dials out.

`cloudflared` keeps a published port 2000 only because Akash rejects a manifest with
`zero global services`. Its metrics server listens on `127.0.0.1:2000`, so that port
answers nothing: the metrics server also serves `/debug/pprof`, `/config` and `/diag`,
and `build-sdl.py` refuses a non-loopback `--metrics`. Check connector health with
`bin/lease-logs.py --service cloudflared` and the Cloudflare dashboard. The image is
pinned by digest (the same build as the backend's); bump both together.

Nothing is exposed `as: 80`: on the first lease that gave a generated hostname that
returned nginx 404 for ten minutes, indistinguishable from one never registered.

**One tunnel per deployment, one live connector per tunnel.** A tunnel's replicas
are chosen by proximity with no traffic steering, so two connectors (an old lease and
a new one sharing `TUNNEL_TOKEN`) split requests between two chains. Before a new
lease connects, the dashboard must show no other connector.

Do not lease the backend on this node's provider: from inside the same provider's
cluster, a provider hostname and NodePort is a hairpin that hangs rather than fails.
The backend reaches the chain through the tunnel's hostnames.

## IBC relayer

Off by default (`ENABLED=false`). It shares the node's image and runs
`docker/relayer.sh` instead of the node entrypoint. Co-locating it with the validator
is safe: a relayer cannot forge packets or move funds, and its key pays gas and holds
nothing else.

To turn it on, set `ENABLED=true` and the counterparty (`COUNTERPARTY_CHAIN_ID`,
`_RPC`, `_PREFIX`, `_GAS_PRICES`, and for Ethereum-style chains `_COIN_TYPE` and
`_EXTRA_CODECS`; the committed values are Injective testnet's). `RELAYER_MNEMONIC`
comes from `.env` and is injected only when you pass `--relayer` (to `create.sh`,
`deploy.sh` or `build-sdl.py`, with the validator's other flags); `build-sdl.py`
refuses `--relayer` unless `ENABLED=true`, and refuses `ENABLED=true` without it. Set `LINK_ON_START=true` for one deploy to create the client,
connection and channel, then put it back: linking spends gas on both chains and a
restart must not retry it. The config is written once into `/data/relayer`, so later
env changes do nothing until that directory is cleared. A new genesis has no IBC
clients, so the old path's ids in the SDL are commented out.

**Fund it on both chains**: uerth to deliver packets here (the node's minimum gas
price applies), the counterparty's token there. A relayer that runs dry on either side
stops silently.

Lessons from the 2026-08-25 link against osmo-test-5 (1 ERTH delivered; ~7,000 uerth
+ ~79,000 uosmo to establish), each of which failed the same way (the relayer looked
healthy, the counterparty balance never moved, and only its logs said why):

- the relayer's own gas price must meet the node's minimum;
- rly v2.6.0 cannot read a chain that hosts 08-wasm light clients (Osmosis does);
  `--override` skips its client scan;
- Osmosis prices fees with an EIP-1559 base fee (x/txfees):
  `/cosmos/base/node/v1beta1/config` reports nothing; read
  `/osmosis/txfees/v1beta1/cur_eip_base_fee` and set well clear of it.

**Client expiry.** A light client dies if not updated within its trusting period, and
an expired client needs governance substitution or a new channel. The link uses 66% of
the counterparty's unbonding period (Hermes' default): about 9 days against Osmosis
mainnet, about 3 against a testnet with 5-day unbonding.

`earth-ibc-test` in the projects folder is the local two-chain rig this came from.
