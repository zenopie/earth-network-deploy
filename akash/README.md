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
the clients use decides the rules:

- The backend indexer reads `/status`, `/blockchain`, `/block_results` and
  `/abci_query` (never with `prove`, never a `cosmos.tx.v1beta1.Service` path).
- Every client polls `GET /cosmos/tx/v1beta1/txs/{hash}` to learn whether its tx
  landed: iOS `EarthClient.awaitCommit` and `PrivacyChain`, Android `PrivacyChain`
  (20 polls at 800 ms), the web app (`src/chain/tx.js`, `explorer.js`), and the
  backend's cosmpy `wait_to_complete` after every gas grant. That is a point read by
  hash and must never be rate limited by the search rule.
- Broadcasts are `POST /cosmos/tx/v1beta1/txs` (no query string); they go through
  the node's mempool checks, not the tx index.
- Only the explorer searches: `GET /cosmos/tx/v1beta1/txs?query=…` (older SDKs:
  `?events=…`). With `tx.height>0` and `ORDER_BY_DESC` that scans the whole tx index.
- Nothing calls `tx_search` or `block_search`, or subscribes.

**Exempting the backend.** The backend reaches both hostnames through Cloudflare from
its Akash provider's egress address, which it may share with the provider's other
tenants. Find it from inside the backend lease (the image has python, not curl):

    bin/lease-shell.py --dseq <backend DSEQ> --service app -- python -c \
      "import urllib.request as u; print(u.urlopen('https://www.cloudflare.com/cdn-cgi/trace').read().decode())" \
      | grep '^ip='

Run it twice a few minutes apart (a provider with several egress addresses shows
more than one) and put every address in a Cloudflare IP list, **Manage Account →
Configurations → Lists**, named `earth_backend_egress` (an IPv6 egress goes in as
its /64). The rules below refer to `$earth_backend_egress`, so a new backend lease
or provider means editing the list, not the rules: re-run the check after every
backend `create.sh`. The cost: the provider's other tenants on the same address are
also exempt from rules 2 and 3. Rule 1 still applies to them, and the node-side
limits above still hold.

Rules (Expression Editor syntax; paste each into "Edit expression"):

1. Custom rule, action **Block**:

       (http.host eq "rpc.erth.network" and (
          http.request.uri.path in {"/tx_search" "/block_search" "/unconfirmed_txs" "/dial_seeds" "/dial_peers"}
          or starts_with(http.request.uri.path, "/unsafe")
          or http.request.uri.query contains "prove=true"
          or http.request.uri.query contains "cosmos.tx.v1beta1.Service"))

   The endpoints that scan an index or touch the peer set, and `prove=true` (a
   Merkle proof per query). `cosmos.tx.v1beta1.Service` closes the side door:
   `/abci_query?path="/cosmos.tx.v1beta1.Service/GetTxsEvent"` runs the same tx
   search as the LCD, through the app's gRPC query router. On a plan with
   request-body fields, add `or http.request.body.raw contains "tx_search"` (and
   the same for `"block_search"`, `"prove":true` and `cosmos.tx.v1beta1.Service`):
   CosmJS sends JSON-RPC as a POST to `/`.

2. Rate limiting rule, **tx search only**, characteristic IP:

       (http.host eq "lcd.erth.network"
        and http.request.method eq "GET"
        and http.request.uri.path eq "/cosmos/tx/v1beta1/txs"
        and any(http.request.uri.args.names[*] in {"query" "events"})
        and not ip.src in $earth_backend_egress)

   120 requests per 1 minute, then block for 1 minute.
   - `path eq`, not "starts with": `/cosmos/tx/v1beta1/txs/{hash}` is a different
     path and never matches. Neither does the broadcast, which is a POST.
   - `args.names` matches the parameter *name* exactly, so `?xquery=` or a hash
     containing "query" cannot trip it, and a search cannot dodge it by reordering
     parameters. If the dashboard refuses `http.request.uri.args.names` on your plan,
     use `(http.request.uri.query contains "query=" or http.request.uri.query contains
     "events=")`. That is stricter (it can match more), never looser.
   - Cloudflare's URL normalization (Rules → Settings → "Normalize incoming URLs",
     on by default) must stay on, or `/cosmos/tx/v1beta1//txs` or a percent-encoded
     path would slip past `eq` while the gRPC gateway still routes it.
   - Sizing: an explorer page view makes 1 to 3 searches. Behind carrier NAT one IPv4
     address fronts many phones, so a per-IP limit is a limit on a whole carrier
     block. 120 per minute is about 40 to 120 explorer views a minute from one
     address, far above what one person does, and a 1-minute block (not 10) means a
     carrier-NAT neighbour who trips it loses search for a minute, not ten. What it
     stops is one address scanning in a loop; the node's query-gas limit bounds each
     scan.

3. Rate limiting rule, **everything else**, characteristic IP:

       ((http.host eq "lcd.erth.network" or http.host eq "rpc.erth.network")
        and not ip.src in $earth_backend_egress)

   1,200 requests per 1 minute, then block for 1 minute.
   - It covers commit polls and broadcasts, which are cheap: a commit poll is a
     point read, and a phone that sends a tx polls at most 20 times. 1,200 a minute
     is 60 phones each sending a tx and polling to the limit in the same minute
     from one carrier address, with room left for their syncs.
   - The backend is exempt because it is one address doing the work of every user:
     a gas grant is a broadcast plus up to a dozen polls, and a re-index from
     height 1 reads every block. Without the exemption a launch-day burst would
     block the backend and every grant after it would come back `202 pending`
     (landed, but unconfirmable) until the block lifted.
   - It is a flood stop, not capacity planning; the node-side limits
     (`EARTHD_API_MAX_OPEN_CONNECTIONS`, `EARTHD_RPC_MAX_OPEN_CONNECTIONS`, query
     gas) are what bound the load on the signer.

4. No caching rule: heights move, and a cached `/status` would stall the indexer.

Plan limits: the Free plan allows one rate limiting rule with a 10-second period.
Rules 2 and 3 as written need a plan with two rules and 1-minute periods (Pro or
above). On Free, keep rule 3 only, as 200 requests per 10 seconds with a 10-second
block, and rely on rule 1 plus the node's query-gas limit for search.

Check from outside (from an address not in the list):

- `curl -s -o /dev/null -w '%{http_code}\n' 'https://rpc.erth.network/tx_search?query="tx.height=1"'`
  prints 403, and `/status` still answers.
- `for i in $(seq 1 150); do curl -s -o /dev/null -w '%{http_code}\n' https://lcd.erth.network/cosmos/tx/v1beta1/txs/<a real tx hash>; done | sort | uniq -c`
  shows only 200: by-hash lookups are not limited by rule 2.
- The same loop on `'/cosmos/tx/v1beta1/txs?query=tx.height%3D1&pagination.limit=1'`
  turns to 429 after 120. Wait a minute before testing anything else from that
  address.
- From the backend lease (same `lease-shell.py` call, a python loop of 1,300 by-hash
  GETs), every status is 200.

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
