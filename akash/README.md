# Akash deployment: the earth chain node

One deployment, four services:

    node          earthd, the validator and full-history RPC   -> /data
    edge          earth-edge, the request filter in front of its RPC and LCD
    cloudflared   Cloudflare Tunnel connector (lcd.*, rpc.* -> edge)
    relayer       IBC relayer, off by default (ENABLED=false)

The backend (gas grants and the privacy indexer) is a separate repo, image and lease
(`earth-network-backend`), with its own tunnel. Closing a lease destroys its volumes,
and this one holds the chain's state: a backend change must not be able to take it.
The image's entrypoint (`docker/entrypoint.sh` in the chain repo) installs the baked
genesis, checks its sha256, and starts `earthd` under cosmovisor. The same image
carries `earth-edge`, which the `edge` service runs instead.

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
debug or trace. earth-edge has no request logging to turn on. The Cloudflare
settings it needs are listed there.

## Public RPC and LCD

`rpc.erth.network` and `lcd.erth.network` are served by the validator process, the
network's only signer. Three layers keep a query flood from slowing block production,
each doing the one thing it can do exactly:

    client -> Cloudflare (per-IP rate limits, no websockets)
           -> cloudflared -> edge (earth-edge: allowlist, per-class caps)
           -> node (query-gas limit, connection caps)

**The filter, `edge`.** earth-edge (chain repo `docker/edge`, built into the node's
image and run as its own service, `akash/deploy.yaml`) is the only thing the public
hostnames reach; the node's 26657 and 1317 are published to it alone. It replaced a
Cloudflare allowlist that read the URI, which could not be made to agree with the
node: CometBFT decodes an `abci_query` argument a second time (`0x` hex, JSON `\u`
escapes; R4-E-1), the gateway routes on a path in which `%2F` is a slash (R4-E-2),
and the GET-only rule broke state sync and every `earthd --node` command (R4-E-3).
The filter:

- **decodes each request once, exactly as the node would** (CometBFT v0.38's URI and
  JSON-RPC argument decoding, grpc-gateway v1.16's path matching), checks the
  decoded call against an allowlist, and **forwards a request it wrote itself** in
  one canonical encoding. The node never parses anything a client wrote, so there
  is no second reading to hide in. Its conformance tests run its decoder against
  CometBFT's own handlers and its routes against every gateway binding in the SDK's
  and the chain's protos;
- **caps each cost class** (below): however many expensive calls arrive, at most a
  few reach the signer at once; the rest get `503` at once;
- bounds bodies (1 MiB), headers (16 KiB) and time, keeps **no per-client state**,
  passes the node only `Origin`, `Accept`, the CORS preflight headers and (LCD)
  `x-cosmos-block-height`, and **logs no request** (NO_LOGS.md);
- answers a refusal with `403` and `refused by the edge filter: <reason>` in the
  node's own error shape, so clients print why.

**On the node** (`akash/deploy.yaml`, refused by `build-sdl.py` when missing):
`EARTHD_QUERY_GAS_LIMIT=50000000` (one gRPC/LCD/abci_query gRPC-path query; the SDK
default is unbounded), `EARTHD_RPC_MAX_OPEN_CONNECTIONS=100`,
`EARTHD_RPC_MAX_SUBSCRIPTION_CLIENTS=0` (no subscriptions at all),
`EARTHD_API_MAX_OPEN_CONNECTIONS=200`, `EARTHD_RPC_UNSAFE=false`. CometBFT 0.38 cannot
switch off `tx_search` or `/websocket`, or meter an index scan or a raw store read
(the kv tx index stays on: every client's commit poll is a by-hash lookup). Those are
the filter's.

**At Cloudflare**: per-IP rate limits and a websocket block, nothing that parses a
request (below).

### What the clients call

Checked against the code (round 4; mobile-orch, app-orch, backend-orch, chain-orch,
docs-privacy). This is the filter's allowlist; anything else is refused.

| Client | `rpc.erth.network` | `lcd.erth.network` |
| --- | --- | --- |
| iOS and Android wallets | GET `/blockchain` (explorer); GET `/status`, `/genesis_chunked`, `/block` (own-node probe) | module GETs (some with `x-cosmos-block-height`), `/cosmos/tx/v1beta1/txs/{hash}` (commit polls), the explorer's search (`message.sender='…'`, `transfer.recipient='…'`); POST `/cosmos/tx/v1beta1/txs` and `/simulate` (JSON) |
| Web app | GET `/blockchain`, `/block_results`, `/abci_query` SupplyOf | module GETs, the explorer's search (`tx.height=N`, sender, recipient; **not** `tx.height>0`, below), POST broadcast (JSON), CORS preflights |
| Keplr (chain suggested by the web app) | GET `/status`. Its send screen opens `/websocket` to wait for the tx: refused, so that screen shows no "confirmed" (the send lands) | GETs, POST broadcast |
| Backend | GET `/status`, `/blockchain`, `/block_results`, `/abci_query` (`/earth.*` trees and handles at a height, `/store/personhood/subspace` `regs_by_dsc`); `earthd gas-check` JSON-RPC POST `/`: `status`, `block`, `abci_query` `/store/{pki,personhood,shielded}/key` (with `prove` when empty) and `/store/pki/subspace` CSCA index ranges | cosmpy: GETs, POST broadcast and simulate |
| `earthd … --node https://rpc.erth.network:443` (docs, trust-store runbook) | JSON-RPC POST `/`: `status`, `block`, `tx` (`query tx`), `abci_query` on the gRPC paths the LCD serves plus `Query/Account`, `Query/ModuleAccountByName`, `Service/Simulate` (`--gas auto`), `Query/Registration`, `Query/RegistrationsByDsc`, `gov.v1 Query/Proposal`; `broadcast_tx_sync`/`_async` | none |
| State sync (`rpc_servers`, docs join.md) | JSON-RPC POST `/`: `commit`, `validators` (`per_page` ≤ 100), `consensus_params` | none |
| Relayer (when enabled) | `http://node:26657` inside the lease, never through Cloudflare or the filter | none |

Not served: `tx_search`, `block_search` (unmetered kv-index scans), `/websocket` and
`subscribe`, JSON-RPC batches, `broadcast_tx_commit`, `check_tx`,
`broadcast_evidence`, `genesis` (use `genesis_chunked`), `net_info`, the consensus
and mempool dumps, the unsafe routes, `abci_query` with `prove` on a gRPC path or a
subspace read, any other `/store/` read, `/app/`, `/p2p/`, `/custom/`, the LCD's own
`abci_query`, `GetTxsEvent`/`GetBlockWithTxs` over abci_query, the LCD search's range
and `events=` forms, form or grpc-web POSTs, `X-HTTP-Method-Override`, and any path
with a `%`-escape, `:verb`, `//`, a trailing `/`, or a `.`/`..` segment.
`earthd query txs` and `query wait-tx` (search, websocket) therefore fail against
the public RPC; `query tx <hash>` works.

**One client change is needed:** the web explorer's "latest transactions" list
searches `tx.height>0`. A range makes CometBFT's kv indexer walk every `tx.height`
entry and sort them, unmetered, so the filter refuses it. The list can be built from
`/blockchain` (`num_txs` per block) and a `tx.height=N` search per block that has
transactions.

### Cost classes

Concurrent calls the filter lets through to the node, per class (`filter/limit.go`),
and how long a call waits for a slot before its `503`:

| Class | What | At once | Waits |
| --- | --- | --- | --- |
| light | status, block, commit, validators, consensus_params, genesis_chunked, tx by hash, LCD blocks and by-hash tx | 24 | 2 s |
| results | block_results, blockchain | 8 | 2 s |
| query | gRPC Query paths (abci_query, LCD GETs) | 12 | 2 s |
| store | raw `/store/` reads | 3 | 2 s |
| broadcast | broadcast_tx_*, LCD POST txs (CheckTx verifies a private tx's proofs) | 4 | 5 s |
| simulate | LCD simulate, abci_query Simulate (runs the tx, proofs included) | 2 | 5 s |
| search | LCD tx search (equality queries only) | 1 | 2 s |

Together that is at most 54 calls at the node, under its 100 RPC and 200 API
connection caps. A changed allowlist or cap is a chain-repo change (`docker/edge`,
with a case in its tests) and a new image; it goes onto the lease with an in-place
PUT like any image change.

### The backend's credential

Every call the backend makes is in the public allowlist above, so the filter neither
needs nor reads a credential (it strips `Authorization`). What the backend still
needs is to be exempt from the **per-IP rate limits**: one address does every user's
gas grants and a full re-index from height 1. So it sends
`Authorization: Basic base64("earth-backend:" + CHAIN_EDGE_TOKEN)` (backend repo
`services/edge.py`, over HTTPS only), and rule 0 skips the rate limiting rules for
that exact header, and nothing else: no custom rule is skipped, so the websocket
block applies, and the filter applies to everyone. A leaked token buys an address
the backend's rate (the per-class caps still bound what reaches the signer), not
the R3-BD-1 harm it bought under the old allowlist (R4-E-6).

Compute the header value from the backend's `.env` on the operator's machine:

    printf 'earth-backend:%s' "$CHAIN_EDGE_TOKEN" | base64 | tr -d '\n'

The value sits in the rule's expression, visible to anyone with dashboard access to the
zone. Rotating it is editing rule 0 and the backend's `.env` together, then an in-place
backend deploy.

### Cloudflare rules

Zone `erth.network`, **Security → WAF**. Order the custom rules 0, 1. Custom rules
run before rate limiting rules.

**Rule 0. Custom rule, action Skip** (the backend, rate limits only):

    (http.host in {"rpc.erth.network" "lcd.erth.network"}
     and http.request.headers["authorization"][0] eq "Basic <value computed above>")

Skip: **All rate limiting rules** only. Not "All remaining custom rules", not BIC or
Security Level (those are off for these hostnames anyway, NO_LOGS.md). Untick
**Log matching requests**.

**Rule 1. Custom rule, action Block** (websockets):

    (http.host in {"rpc.erth.network" "lcd.erth.network"} and (
       http.request.uri.path eq "/websocket"
       or any(lower(http.request.headers.names[*])[*] eq "upgrade")))

The filter refuses an upgrade too; this keeps the socket from being opened past
Cloudflare at all, where a rate limit would count it once. (**Network → WebSockets**
off for the zone does the same, if nothing else on `erth.network` uses one; the
backend and the web app do not.)

**Rule 2. Rate limiting rule, tx search**, characteristic IP:

    (http.host eq "lcd.erth.network" and http.request.method eq "GET"
     and http.request.uri.path eq "/cosmos/tx/v1beta1/txs")

120 requests per 1 minute, then block for 1 minute. The filter gives the search one
slot for everyone, so without a per-IP limit one address could keep it busy and
everyone else would get `503`. The path is the decoded one only after the filter has
refused `%`-escapes, so `/cosmos/tx%2Fv1beta1/txs` cannot dodge this rule into
rule 3 and still be served.

**Rule 3. Rate limiting rule, everything else**, characteristic IP:

    (http.host in {"lcd.erth.network" "rpc.erth.network"})

1,200 requests per 1 minute, then block for 1 minute. A flood stop for one address,
not capacity planning: behind carrier NAT one IPv4 address fronts many phones (a
commit poll is a point read; a phone that sends a tx polls at most 20 times), and the
filter's caps are what bound the signer's load. The backend skips it (rule 0).

No caching rule: heights move, and a cached `/status` would stall the indexer.

### What each plan accepts

Cloudflare's availability tables, as documented (verify in the dashboard; it refuses a
field the plan lacks, loudly):

| | Free | Pro | Business |
| --- | --- | --- | --- |
| Custom rules (host, path, headers, `any`, `lower`, Skip) | 5 | 20 | 100 |
| Rate limiting rules | 1 | 2 | 5 |
| Rate limiting expression fields | path | host, path, URI, query | adds method, source IP, user agent |
| Periods / block durations | 10 s / 10 s | up to 1 min / up to 1 h | up to 10 min / up to 1 day |

Rules 0 and 1 are the same on every plan. Rules 2 and 3:

- **Business**: as written.
- **Pro**: rule 3 as written; rule 2 without the method (a search always has a query
  string, a broadcast none):

      (http.host eq "lcd.erth.network" and http.request.uri.path eq "/cosmos/tx/v1beta1/txs"
       and http.request.uri.query ne "")

- **Free**: one rule, path fields only, so no host. Keep rule 3, scoped by the node's
  paths so it never touches `api.erth.network` or the web app, at 200 requests per
  10 seconds with a 10-second block:

      (starts_with(http.request.uri.path, "/cosmos/") or starts_with(http.request.uri.path, "/earth/")
       or http.request.uri.path in {"/" "/status" "/block" "/blockchain" "/block_results" "/abci_query"
                                    "/genesis_chunked" "/commit" "/validators" "/consensus_params" "/tx"})

  The search then has only that limit and its one slot.

Nothing here depends on URL normalization any more (the filter refuses what
normalization would rewrite); leaving it on is harmless.

### Check from outside

From an address that does not hold the token. Expect `403` with `refused by the edge
filter` unless noted. A `200` on any of the refused lines, or a `tx_search` that
answers, means the hostname reaches the node directly instead of `edge`: fix the
Public Hostname before anything else.

    R=https://rpc.erth.network L=https://lcd.erth.network
    c() { curl -s -o /dev/null -w '%{http_code}\n' "$@"; }
    j() { curl -s -H 'content-type: application/json' -d "$1" "$R/"; echo; }
    # served
    c "$R/status"                                                                     # 200
    c "$R/abci_query?path=%22/cosmos.bank.v1beta1.Query/SupplyOf%22&data=0x0a057565727468&height=1"   # 200
    j '{"jsonrpc":"2.0","id":1,"method":"status"}' | head -c 60                        # a result
    j '{"jsonrpc":"2.0","id":1,"method":"commit","params":{}}' | head -c 60           # a result (state sync)
    earthd status --node $R:443 | head -c 60                                          # a result
    earthd query auth module-account gov --node $R:443                               # the gov account
    # refused: scans, sockets, batches
    c "$R/tx_search?query=%22tx.height%3E0%22"
    j '{"jsonrpc":"2.0","id":1,"method":"tx_search","params":{"query":"tx.height>0"}}'
    j '[{"jsonrpc":"2.0","id":1,"method":"status"}]'
    c -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
      -H "Sec-WebSocket-Key: $(openssl rand -base64 16)" "$R/websocket"           # 403 (rule 1)
    # refused: abci_query encodings (R3-BD-1, R4-E-1)
    c "$R/abci_query?path=%22/cosmos.bank.v1beta1.Query/SupplyOf%22&data=0x0a057565727468&prove=%74rue"
    c "$R/abci_query?path=0x2f636f736d6f732e74782e763162657461312e536572766963652f4765745478734576656e74&x=/cosmos.bank.v1beta1.Query/SupplyOf"
    c "$R/abci_query?path=%22/cosmos.tx.v1beta1.Ser%5Cu0076ice/GetTxsEvent%22&x=/cosmos.bank.v1beta1.Query/SupplyOf"
    c "$R/abci_query?path=%22/%5Cu0073tore/bank/subspace%22"
    c "$R/abci_query?path=0x2f73746f72652f62616e6b2f7375627370616365"
    # refused: LCD side doors (R3-BD-1, R4-E-2)
    c "$L/cosmos/base/tendermint/v1beta1%2Fabci_query?path=/store/bank/subspace&data="
    c "$L/cosmos/base/tendermint/v1beta1/abci_query?path=/store/bank/subspace"
    c "$L/cosmos/tx%2Fv1beta1/txs?query=tx.height%3E0"
    c "$L/cosmos/tx/v1beta1/txs?query=tx.height%3E0"
    c -X POST -H 'content-type: application/x-www-form-urlencoded' -H 'X-HTTP-Method-Override: GET' \
      --data 'query=tx.height%3E0' "$L/cosmos/tx/v1beta1/txs"
    c "$L/cosmos/tx/v1beta1/txs?query=tx.height%3D1"                                  # 200 (equality)

Then the rate limits:

- `for i in $(seq 1 150); do curl -s -o /dev/null -w '%{http_code}\n' $L/cosmos/tx/v1beta1/txs/<a real tx hash>; done | sort | uniq -c`
  shows only 200: by-hash lookups are not limited by rule 2.
- The same loop on `'/cosmos/tx/v1beta1/txs?query=tx.height%3D1&limit=1'` turns to
  429 after 120 (and may show a few 503 if another search held the slot). Wait a
  minute before testing anything else from that address.

And with the token, from the operator's machine (the backend's `.env` sourced): 1,300
`/status` GETs with `-u "earth-backend:$CHAIN_EDGE_TOKEN"` all answer 200, and a real
registration's `/gas/register` succeeds. In **Security → Events**, none of the token's
requests appear.

## Addresses

The node is reachable for clients **only through the tunnel and the filter**:
`lcd.erth.network` and `rpc.erth.network`. Neither 1317 nor 26657 is on a provider
port, so a new lease changes no client address. Configure the Public Hostnames on
Cloudflare's side, at the filter, never the node:

    lcd.* -> http://edge:1317      rpc.* -> http://edge:26657

`node:*` would also answer (the lease's services can reach each other by name), and
would serve the public with no filter at all. "Check from outside" catches it.

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
