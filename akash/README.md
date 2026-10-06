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
`EARTHD_QUERY_GAS_LIMIT=50000000` (one gRPC/LCD/abci_query gRPC-path query; the SDK
default is unbounded), `EARTHD_RPC_MAX_OPEN_CONNECTIONS=100`,
`EARTHD_RPC_MAX_SUBSCRIPTION_CLIENTS=0` (no subscriptions at all),
`EARTHD_API_MAX_OPEN_CONNECTIONS=200`, `EARTHD_RPC_UNSAFE=false`. These are env, so
changing one is an in-place PUT. What CometBFT 0.38 cannot do on the node: switch off
`tx_search`/`block_search` (the kv tx index has to stay on, because every client's
commit poll is a by-hash `/tx` lookup through the LCD), switch off `/websocket`, or
meter an index scan or a `/store/<module>/subspace` read (neither runs under the
query-gas meter). Those are closed at Cloudflare.

### What the clients call

Checked against the code (round 3, R3-BD-1). Anything not listed here is refused.

| Client | `rpc.erth.network` | `lcd.erth.network` |
| --- | --- | --- |
| iOS and Android wallets | GET `/blockchain` (explorer range); GET `/status`, `/genesis_chunked`, `/block` (own-node probe) | GET module queries and `/cosmos/tx/v1beta1/txs/{hash}` (commit polls); POST `/cosmos/tx/v1beta1/txs` and `/cosmos/tx/v1beta1/simulate` (JSON) |
| Web app | GET `/blockchain`, `/block_results`, `/abci_query?path="/cosmos.bank.v1beta1.Query/SupplyOf"` | the same GETs, the explorer's search `GET /cosmos/tx/v1beta1/txs?query=…`, POST `/cosmos/tx/v1beta1/txs` (JSON); browser CORS preflights (`OPTIONS`) |
| Keplr (chain suggested by the web app) | GET `/status`. Its own send screen also opens `/websocket` to wait for the tx; refused, so that one screen shows no "confirmed" notice (the send itself goes through the LCD and lands) | GETs, POST broadcast |
| Backend | GET `/status`, `/blockchain`, `/block_results`, `/abci_query` (`/earth.*`, `/store/personhood/subspace`); **JSON-RPC POST `/`** from `earthd gas-check` (`status`, `block`, `abci_query` with `prove=true`) | cosmpy GETs, POST broadcast and simulate |
| Relayer (when enabled) | `http://node:26657` inside the lease, never through Cloudflare | none |

Nothing of ours calls `tx_search` or `block_search`, subscribes, or sends a form-encoded
POST to the LCD. The backend is the one client that needs more than the public set (a
POST, `prove`, raw store reads, and one address doing every user's work), so it carries
a credential instead of being let through by address.

### The backend's credential

The backend sends `Authorization: Basic base64("earth-backend:" + CHAIN_EDGE_TOKEN)` to
both hostnames, and only to them (backend repo `services/edge.py`; its
`deploy/akash/README.md`, "The edge token"). Rule 0 matches that exact header. This
replaces the old egress-IP list: an Akash provider's egress address is shared with its
other tenants and changes with the lease; the token is neither. Compute the header
value from the backend's `.env` on the operator's machine:

    printf 'earth-backend:%s' "$CHAIN_EDGE_TOKEN" | base64 | tr -d '\n'

The value sits in the rule's expression, visible to anyone with dashboard access to the
zone, which is the same set of people who could edit the rules anyway. Rotating it is
editing rule 0 and the backend's `.env` together, then an in-place backend deploy.

### Rules

Zone `erth.network`, **Security → WAF**. Paste each expression into "Edit expression".
Custom rules run before rate limiting rules, so rule 0's Skip covers both. Order the
custom rules 0, 1, 2. URL normalization (**Rules → Settings → Normalize incoming
URLs**, on by default) must stay on: the path checks below compare normalized paths.

**Rule 0. Custom rule, action Skip** (the backend):

    (http.host in {"rpc.erth.network" "lcd.erth.network"}
     and http.request.headers["authorization"][0] eq "Basic <value computed above>")

Skip: **All remaining custom rules**, **All rate limiting rules**, and under "More
components to skip" Browser Integrity Check and Security Level. Untick **Log matching
requests**: the backend's every request would otherwise be a Security Event.

**Rule 1. Custom rule, action Block** (RPC allowlist):

    (http.host eq "rpc.erth.network" and (
       http.request.method ne "GET"
       or not http.request.uri.path in {"/status" "/block" "/blockchain" "/block_results" "/abci_query" "/genesis_chunked"}
       or (http.request.uri.path eq "/abci_query" and (
            not lower(url_decode(http.request.uri.query)) contains "/cosmos.bank.v1beta1.query/supplyof"
            or lower(url_decode(http.request.uri.query)) contains "prove"
            or lower(url_decode(http.request.uri.query)) contains "service"
            or lower(url_decode(http.request.uri.query)) contains "/store"
            or lower(url_decode(http.request.uri.query)) contains "/app"
            or lower(url_decode(http.request.uri.query)) contains "/p2p"
            or lower(url_decode(http.request.uri.query)) contains "/custom"))))

- **Method.** CometBFT serves JSON-RPC from a POST body to `/` (and from a GET with a
  body), and serves every URI route to any method. Only GET passes, and `/` is not on
  the list, so no JSON-RPC body reaches the node from the public.
- **Paths.** An allowlist, so `/websocket` (which carries every RPC method, `tx_search`
  included, over one upgrade that counts as one request), `/tx_search`,
  `/block_search`, `/unconfirmed_txs`, `/net_info`, `/dump_consensus_state`,
  `/broadcast_tx_*`, the unsafe routes and anything added by a future CometBFT are all
  refused without being named.
- **`/abci_query`.** The query string reaches Cloudflare raw and the node decodes it,
  so a check on the raw string is evaded by `prove=%74rue`. The checks run on
  `lower(url_decode(…))`, one decoding, as Go's `url.Query()` does once, and lowercased,
  which only widens them (the node's names and paths are case-sensitive). They are a
  blocklist over the whole decoded string, not a parse, because CometBFT reads the
  first of repeated parameters: `prove` in any position, any `…Service/` gRPC path
  (`cosmos.tx.v1beta1.Service/GetTxsEvent` runs the tx search; the CometBFT service
  and reflection are services too), raw `/store/` reads (a `subspace` read with an
  empty prefix returns a whole module store, unmetered), `/app/` (simulate), `/p2p/`
  and `/custom/`. What passes is a gRPC `…Query/…` path, metered by the query-gas
  limit and no more than the LCD already serves; the SupplyOf requirement keeps
  casual use to the web app's one call. `data` is hex and `height` digits, so neither
  can hold the blocked words for a real caller.
- If the dashboard refuses `url_decode` on your plan, take `"/abci_query"` out of the
  path list and delete the `/abci_query` clause: the public then has no abci_query at
  all (the web app's `supplyAtHeight` returns null and the explorer leaves its issuance
  figure blank; the backend is unaffected through rule 0). A raw-string check is not
  an alternative: the web app's own call is percent-encoded (`%22`), so no raw
  `contains` can tell it from an encoded attack. On Business or above, `matches`
  (regex) allows an exact positive match on the decoded string instead.

**Rule 2. Custom rule, action Block** (LCD side doors):

    (http.host eq "lcd.erth.network" and (
       not http.request.method in {"GET" "POST" "OPTIONS"}
       or (http.request.method eq "POST"
           and not http.request.uri.path in {"/cosmos/tx/v1beta1/txs" "/cosmos/tx/v1beta1/simulate"})
       or any(http.request.headers["content-type"][*] contains "form-urlencoded")
       or any(lower(http.request.headers.names[*])[*] eq "x-http-method-override")
       or starts_with(http.request.uri.path, "/cosmos/base/tendermint/v1beta1/abci_query")))

- **The method-override door.** The SDK's gRPC gateway (grpc-gateway v1.16.0,
  `runtime/mux.go`, path-length fallback on by default) turns a POST with
  `Content-Type: application/x-www-form-urlencoded` and `X-HTTP-Method-Override: GET`
  into a GET, with its parameters in the body: `POST /cosmos/tx/v1beta1/txs` becomes
  the tx search, invisible to any check on the method or the query string, and a
  form POST to any GET-only path runs that GET. No client sends form bodies or the
  override header (every POST is `application/json`), so both are refused, and POST
  is limited to broadcast and simulate. `OPTIONS` stays for the web app's CORS
  preflights.
- **`/cosmos/base/tendermint/v1beta1/abci_query`.** The LCD's abci_query refuses gRPC
  paths (cmtservice `ABCIQuery`) but serves `/store/…` reads, `subspace` and `prove`
  included. No client uses it.
- The LCD has no websocket, and its tx search is GET-only once the door above is shut,
  which is what makes rule 3's `path eq` complete.

**Rule 3. Rate limiting rule, tx search only**, characteristic IP:

    (http.host eq "lcd.erth.network" and http.request.method eq "GET"
     and http.request.uri.path eq "/cosmos/tx/v1beta1/txs")

120 requests per 1 minute, then block for 1 minute.

- GET on that exact path is only ever the search: `/cosmos/tx/v1beta1/txs/{hash}` is
  another path (a trailing slash routes to the by-hash lookup with an empty hash, and
  `txs:verb` is a 404), and the broadcast is a POST. So nothing about the parameters
  needs matching, and nothing can dodge the rule by spelling them differently
  (`?%71uery=` is still a GET on this path). The earlier `args.names` clause added only
  that evasion and is gone (R3-BD-2).
- Sizing: an explorer page view makes 1 to 3 searches. Behind carrier NAT one IPv4
  address fronts many phones, so a per-IP limit is a limit on a whole carrier block.
  120 per minute is about 40 to 120 explorer views a minute from one address, far
  above what one person does, and a 1-minute block (not 10) means a carrier-NAT
  neighbour who trips it loses search for a minute, not ten. What it stops is one
  address scanning in a loop.

**Rule 4. Rate limiting rule, everything else**, characteristic IP:

    (http.host in {"lcd.erth.network" "rpc.erth.network"})

1,200 requests per 1 minute, then block for 1 minute.

- It covers commit polls and broadcasts, which are cheap: a commit poll is a point
  read, and a phone that sends a tx polls at most 20 times. 1,200 a minute is 60
  phones each sending a tx and polling to the limit in the same minute from one
  carrier address, with room left for their syncs.
- The backend skips it (rule 0): a gas grant is a broadcast plus up to a dozen polls,
  and a re-index from height 1 reads every block. Without the skip a launch-day burst
  would block the backend and every grant after it would come back `202 pending`.
- It is a flood stop, not capacity planning; the node-side limits are what bound the
  load on the signer.

No caching rule: heights move, and a cached `/status` would stall the indexer.

### What each plan accepts

Cloudflare's availability tables, as documented (verify in the dashboard; it refuses a
field the plan lacks, loudly):

| | Free | Pro | Business |
| --- | --- | --- | --- |
| Custom rules | 5 | 20 | 100 |
| Custom rule fields used here (host, method, path, query, headers) and functions (`lower`, `url_decode`, `any`, `starts_with`), Skip action | yes | yes | yes |
| `matches` (regex) | no | no | yes |
| Rate limiting rules | 1 | 2 | 5 |
| Rate limiting expression fields | path | host, path, URI, query | adds method, source IP, user agent |
| Periods / block durations | 10 s / 10 s | up to 1 min / up to 1 h | up to 10 min / up to 1 day |

So rules 0 to 2, which carry the protection, are the same on every plan. Rules 3 and 4:

- **Business**: as written.
- **Pro**: rule 4 as written. Rule 3 cannot name the method, so use the query string
  instead (a search always has one; a broadcast has none):

      (http.host eq "lcd.erth.network" and http.request.uri.path eq "/cosmos/tx/v1beta1/txs"
       and http.request.uri.query ne "")

- **Free**: one rule, path fields only, so no host. Keep rule 4 only, scoped by the
  node's paths so it never touches `api.erth.network` or the web app (neither serves
  any of these), at 200 requests per 10 seconds with a 10-second block:

      (starts_with(http.request.uri.path, "/cosmos/") or starts_with(http.request.uri.path, "/earth/")
       or starts_with(http.request.uri.path, "/ibc/") or starts_with(http.request.uri.path, "/cosmwasm/")
       or http.request.uri.path in {"/status" "/block" "/blockchain" "/block_results" "/abci_query" "/genesis_chunked"})

  Search then has only that limit and the node's own bounds. At about 1.25 polls a
  second per phone, 200 per 10 s allows about 16 phones polling at once behind one
  address; a trip costs 10 s, which the wallets' 20 × 800 ms poll loop partly absorbs.

### Check from outside

From an address that does not hold the token (expect `403` unless noted):

    R=https://rpc.erth.network L=https://lcd.erth.network
    c() { curl -s -o /dev/null -w '%{http_code}\n' "$@"; }
    c "$R/status"                                                    # 200
    c "$R/tx_search?query=%22tx.height%3E0%22"
    c -X POST -H 'content-type: application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"tx_search","params":{"query":"tx.height>0"}}' "$R/"
    c -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
      -H "Sec-WebSocket-Key: $(openssl rand -base64 16)" "$R/websocket"
    c "$R/abci_query?path=%22/cosmos.bank.v1beta1.Query/SupplyOf%22&data=0x0a057565727468&prove=%74rue"
    c "$R/abci_query?path=%22/cosmos%2Etx%2Ev1beta1%2EService/GetTxsEvent%22"
    c "$R/abci_query?path=%22/store/bank/subspace%22"
    c "$R/abci_query?path=%22/cosmos.bank.v1beta1.Query/SupplyOf%22&data=0x0a057565727468"   # 200
    c -X POST -H 'content-type: application/x-www-form-urlencoded' -H 'X-HTTP-Method-Override: GET' \
      --data 'query=tx.height%3E0' "$L/cosmos/tx/v1beta1/txs"
    c "$L/cosmos/base/tendermint/v1beta1/abci_query?path=/store/bank/subspace"

Then the rate limits:

- `for i in $(seq 1 150); do curl -s -o /dev/null -w '%{http_code}\n' $L/cosmos/tx/v1beta1/txs/<a real tx hash>; done | sort | uniq -c`
  shows only 200: by-hash lookups are not limited by rule 3.
- The same loop on `'/cosmos/tx/v1beta1/txs?query=tx.height%3D1&pagination.limit=1'`
  turns to 429 after 120. Wait a minute before testing anything else from that
  address.

And with the token, from the operator's machine (the backend's `.env` sourced):

    curl -s -u "earth-backend:$CHAIN_EDGE_TOKEN" -H 'content-type: application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"status"}' https://rpc.erth.network/ | head -c 80   # a result

and a real registration's `/gas/register` succeeding (gas-check is the JSON-RPC POST).
In **Security → Events**, none of the token's requests appear (rule 0 logs nothing).

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
