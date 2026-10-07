# Akash deployment: the earth chain node

One deployment, four services:

    node          earthd, the validator and full-history RPC   -> /data
    edge          earth-edge, the request filter in front of its RPC and LCD
    cloudflared   Cloudflare Tunnel connector (lcd.*, rpc.* -> edge)
    relayer       IBC relayer, off by default (ENABLED=false)

The backend (gas grants and the privacy indexer) is a separate repo, image and lease
(`earth-network-backend`), with its own tunnel. Closing a lease destroys its volumes,
and this one holds the chain's state: a backend change must not be able to take it.
The node image's entrypoint (`node/entrypoint.sh`) installs the baked genesis, checks
its sha256, injects the consensus and node keys, refuses a foreign genesis or a
leftover consensus key, and starts `earthd` under cosmovisor. The relayer runs the
same image. `edge` runs its own (below).

## The images

**node and relayer:** this repo's `node/` (`node/Dockerfile`), built `FROM` the
chain's generic image by digest. The chain repo's CI builds that image on
`v[0-9]+.[0-9]+.[0-9]+` tags only: `earthd`, its libraries, the release genesis and
a minimal entrypoint, nothing of ours. `node/base.pin` names the chain image
(`bin/build-node.sh --base <tag>`); `bin/build-node.sh --pin` builds ours on it (the
entrypoint, `relayer.sh` and `drop-root.sh` from `node/`, cosmovisor and rly built
in), pushes `ghcr.io/zenopie/earth-network-node:<commit>` and writes its digest,
with the base as a `# FROM` comment, on the `node` and `relayer` lines of
`akash/deploy.yaml`. That digest is committed. `bin/deploy.sh` / `bin/create.sh
<tag>` resolve the chain tag (`bin/digest.sh`), and `build-sdl.py` refuses unless it
is `node/base.pin` and both lines' `FROM`, so what deploys is the node image built
on that release. It also refuses the placeholder, the chain image itself and a
`command` on `node`. `node/entrypoint_test.sh` tests the entrypoint without a
container. RELAUNCH.md 1.5 is the procedure.

**edge:** built from this repo's `edge/` (`edge/Dockerfile`: a static binary on
`scratch`, uid 65532, nothing else) by `bin/build-edge.sh --pin`, which pushes
`ghcr.io/zenopie/earth-network-edge:<commit>` and writes its digest into
`akash/deploy.yaml`. That digest **is** committed: it names a commit of this repo,
not a chain release. `build-sdl.py` refuses the all-zero placeholder in any build
that pins the node, a tag instead of a digest, and a `command` or `args` on `edge`.
RELAUNCH.md 1.4 is the procedure, including the conformance run against the chain
commit `edge/conformance/chain.pin` names.

All three packages (earth-network-chain, -node, -edge) must be public on ghcr.io:
the provider pulls the node and edge images, and the node build pulls the chain's.

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

**The filter, `edge`.** earth-edge (`edge/` in this repo, its own image, run as
its own service, `akash/deploy.yaml`) is the only thing the public
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
  few reach the signer at once; the rest get `503`. A slot is held until the
  node has answered, not until the client gives up: the node cannot be told to stop
  a query, so a slot freed at a timeout would let the work pile up (R5-E-1, R5-E-3);
- **lets through only calls whose cost is bounded**, not only calls of an allowed
  kind (round 5): the tx search answers `tx.height=N` only (CometBFT loads every
  match of a search before it pages, so an address search is a scan of that
  address's whole history, and the fee collector's is every tx); every paginated
  route gets an explicit `pagination.limit` (1..1000) and refuses `count_total=true`
  and offsets over 10,000; `abci_query` serves a short list of unpaginated paths and
  not `Simulate`;
- **models the node's real bottleneck**: CometBFT runs the app behind one mutex that
  consensus also waits on. Every public call that takes it (RPC `abci_query`, both
  broadcasts, `abci_info`) is in one of two classes, reads and broadcasts, 1 public
  slot and 1 backend slot each: at most 4 holders, and a read flood cannot starve
  broadcasts;
- **caps answers sized by chain data** (`block_results`, a tx by hash): a class
  each and a byte ceiling per call (below), `block_results` small and separate from
  the wallets' commit polls; the chain's per-tx result cap is the real bound;
- bounds bodies (1 MiB each; read before any slot is taken, under a deadline of
  2 s + 128 KiB/s of their size; charged, copies included, to a 48 MiB byte budget,
  16 MiB for the backend, until answered; reserved in chunks that grow with what
  has arrived, starting at 4 KiB, so a silent body holds 24 KiB), headers (16 KiB) and time,
  keeps **no per-client state but an in-memory count** of each client's requests in
  flight per class (below; deleted when it reaches zero, NO_LOGS.md),
  passes the node only `Origin`, `Accept`, the CORS preflight headers and (LCD)
  `x-cosmos-block-height`, and **logs no request** (NO_LOGS.md);
- answers a refusal with `403` and `refused by the edge filter: <reason>` in the
  node's own error shape, so clients print why;
- sets `X-Earth-Edge: 1` on every answer, served or refused. The node never does,
  so `bin/check-edge.py` can tell from outside which one a hostname reaches.

**On the node** (`akash/deploy.yaml`, refused by `build-sdl.py` when missing):
`EARTHD_QUERY_GAS_LIMIT=50000000` (one gRPC/LCD/abci_query gRPC-path query; the SDK
default is unbounded), `EARTHD_RPC_MAX_OPEN_CONNECTIONS=100`,
`EARTHD_RPC_MAX_SUBSCRIPTION_CLIENTS=0` (no subscriptions at all),
`EARTHD_API_MAX_OPEN_CONNECTIONS=200`, `EARTHD_RPC_UNSAFE=false`,
`EARTHD_WASM_SIMULATION_GAS_LIMIT=10000000` (a simulate needs no fee; unset, wasmd
lets one burn the 100M block gas, ~15 s of contract CPU; private txs are simulated
up to the block limit regardless, chain `app/ante.go`), and `EARTHD_INDEX_EVENTS`
set to the IBC packet keys only, so no address event is indexed and an address
search finds nothing even if it reaches the node (the relayer's packet queries
still work; `tx.hash` and `tx.height` are always indexed). CometBFT 0.38 cannot
switch off `tx_search` or `/websocket`, or meter or cancel an index scan or a raw
store read (the kv tx index stays on: every client's commit poll is a by-hash
lookup). Those are the filter's.

**At Cloudflare**: per-IP rate limits and a websocket block, nothing that parses a
request (below).

### What the clients call

Checked against the code (round 4; mobile-orch, app-orch, backend-orch, chain-orch,
docs-privacy). This is the filter's allowlist; anything else is refused.

| Client | `rpc.erth.network` | `lcd.erth.network` |
| --- | --- | --- |
| iOS and Android wallets | GET `/blockchain` (explorer); GET `/status`, `/genesis_chunked`, `/block` (own-node probe) | module GETs (some with `x-cosmos-block-height`), `/cosmos/tx/v1beta1/txs/{hash}` (commit polls, and the activity list: the txs the wallet sent, looked up by hash); POST `/cosmos/tx/v1beta1/txs` and `/simulate` (JSON) |
| Web app | GET `/blockchain`, `/block_results`, `/abci_query` SupplyOf | module GETs, the explorer's search (`tx.height=N` only; its address page lists no history), POST broadcast (JSON), CORS preflights |
| Keplr (chain suggested by the web app) | GET `/status`. Its send screen opens `/websocket` to wait for the tx: refused, so that screen shows no "confirmed" (the send lands) | GETs, POST broadcast |
| Backend | GET `/status`, `/blockchain`, `/block_results`, `/abci_query` (`/earth.*` trees and handles at a height, `/store/personhood/subspace` `regs_by_dsc`); `earthd gas-check` JSON-RPC POST `/`: `status`, `block`, `abci_query` `/store/{pki,personhood,shielded}/key` (with `prove` when empty) and `/store/pki/subspace` CSCA index ranges | cosmpy: GETs, POST broadcast and simulate |
| `earthd … --node https://rpc.erth.network:443` (docs, trust-store runbook) | JSON-RPC POST `/`: `status`, `block`, `tx` (`query tx`), `abci_query` on `auth Query/Account`, `Query/ModuleAccountByName`, `gov.v1 Query/Proposal`, `personhood Query/Registration`, `Query/RegistrationsByDsc`, `assembly Query/ProposalTally`, `bank Query/SupplyOf` and the backend's tree and handle queries; `broadcast_tx_sync`/`_async`. **Not `Service/Simulate`**: `earthd tx` against the public RPC takes an explicit `--gas` (or simulate through the LCD) | none |
| State sync (`rpc_servers`, docs join.md) | JSON-RPC POST `/`: `commit`, `validators` (`per_page` ≤ 100), `consensus_params` | none |
| Relayer (when enabled) | `http://node:26657` inside the lease, never through Cloudflare or the filter | none |

Not served: `tx_search`, `block_search` (unmetered kv-index scans), `/websocket` and
`subscribe`, JSON-RPC batches, `broadcast_tx_commit`, `check_tx`,
`broadcast_evidence`, `genesis` (use `genesis_chunked`), `net_info`, the consensus
and mempool dumps, the unsafe routes, `abci_query` with `prove` on a gRPC path or a
subspace read, any other `/store/` read, `/app/`, `/p2p/`, `/custom/`, the LCD's own
`abci_query`, any gRPC path over `abci_query` outside the list above (`Simulate`,
`GetTxsEvent`, every paginated query: use the LCD), the LCD search's address
(`message.sender`, `transfer.recipient`), range and `events=` forms,
`pagination.count_total=true`, a `pagination.limit` of 0 or over 1,000, an offset
over 10,000, form or grpc-web POSTs, `X-HTTP-Method-Override`, and any path with a
`%`-escape, `:verb`, `//`, a trailing `/`, or a `.`/`..` segment. `earthd query txs`
and `query wait-tx` (search, websocket) therefore fail against the public RPC;
`query tx <hash>` works.

**Address history is not served.** No public endpoint of this chain lists an
address's transactions: a search for one is a scan the signer cannot bound or cancel,
and an index keyed by address is the record "people are private" rules out. The
wallets list the txs they sent themselves (kept on the device, looked up by hash);
the explorer's address page shows balances and state, not history. Anyone who wants
an address search runs their own node with the kv indexer and `index-events` empty.

### Cost classes

Concurrent calls the filter lets through to the node, per class (`filter/limit.go`):
public slots, the backend's reserved slots, and how long a call waits for a slot
before its `503`. A slot is held until the node answers; the client gets `504` at the
class's deadline but the slot stays taken until then. It is also held while the
answer streams to the client, but a client that stops reading is cut off after 10 s
without progress (45 s for the whole answer), and the slot is freed as soon as the
node's answer has been read to its end.

| Class | What | Public | Per client | Backend | Waits |
| --- | --- | --- | --- | --- | --- |
| light | status, block, commit, validators, consensus_params, LCD blocks, preflights | 12 | 3 | +3 | 2 s |
| results | blockchain, genesis_chunked | 8 | 2 | +4 | 2 s |
| bulk | `block_results` (one block's results, up to ~10 MB for a block built to be large) | 2 | 1 | +1 | 2 s |
| txhash | a tx by hash: RPC `tx`, LCD `txs/{hash}` (wallet commit polls, activity refresh) | 8 | 2 | +2 | 2 s |
| query | LCD gRPC GETs (outside the ABCI mutex, metered by query gas) | 12 | 3 | +4 | 2 s |
| abci-query | RPC `abci_query` (gRPC paths and `/store/` reads), `abci_info`: ABCI mutex | 1 | 1 | +1 | 4 s |
| broadcast | `broadcast_tx_sync`/`_async`, LCD POST txs (CheckTx): ABCI mutex | 1 | 1 | +1 | 4 s |
| simulate | LCD simulate (CPU outside the mutex, bounded by `simulation_gas_limit`) | 2 | 1 | +1 | 4 s |
| search | LCD tx search, `tx.height=N` only | 1 | 1 | 0 | 2 s |

**Per client** (round-8 R8-D-1) is how many requests one client may have in a
class at once, holding a slot or queued for one; past it the request gets `503` at
once. Slots are granted in arrival order. So two clients together hold at most half
of any class of 4 or more slots, and in the 1- and 2-slot classes a third client's
request is next after the requests already queued, at most one per other client
(`edge/filter` `TestTwoClientsCannotStarveAThird`: txhash, bulk, both ABCI-mutex
classes, light). Cloudflare's per-IP rate limits count requests, not what they hold,
so this is what stops one or two addresses from keeping a class full with slow
requests. The client is `CF-Connecting-IP`: the edge's ports are published to the
lease's cloudflared only (`build-sdl.py` refuses anything else), and Cloudflare sets
that header itself on every request it proxies, so the edge reads it as Cloudflare's
word. IPv6 counts by /64. One clean IP address is used; no header, two, or garbage
counts as the tunnel connection's own address, which every such request shares, so a
bad header never buys a fresh allowance (`edge/filter/client.go`). Clients sharing
an address (carrier NAT, an office) share its allowance; the allowance is
concurrency, not rate, so ordinary wallet traffic (millisecond calls) rarely meets
it. The backend's credential is exempt and keeps its reserve. The counts are in
memory only, keyed by a keyed hash of the address, and a client's entry is deleted
when its last request is answered (NO_LOGS.md).

Together at most 64 calls at the node, under its 100 RPC and 200 API connection caps
and within the edge's 64 connections per upstream (a test holds the classes to it).
A tx by hash has its own class (round-7 R7-D-1): its answer is bounded by the chain's
1 MiB per-tx result cap, so it does not need `block_results`' tiny class, and a flood
of heavy `block_results` reads cannot make a committed tx look unconfirmed to the
wallets. At most 4 of them hold or queue on the
ABCI mutex, so a consensus step waits behind at most four bounded calls; reads and
broadcasts have separate slots, so a flood of `abci_query` cannot refuse wallet
broadcasts (round-6 R6-E-3).

**Answer ceilings** (round-6 R6-E-1). A public answer to RPC `tx` is cut off past
8 MiB, LCD `txs/{hash}` past 12 MiB, `block_results` and the `tx.height=N` search past
32 MiB, RPC `block` past 12 MiB and LCD blocks past 24 MiB (declared larger: `502`;
streamed past it: the connection is aborted). The backend has no ceiling. This is a
backstop: the node has built the whole answer before the edge counts a byte. What
bounds the build is the chain: block `max_bytes` 4 MiB (genesis), a tx's stored
result capped at 1 MiB (about 10 MB of results per block at worst), and a default
node admitting txs of up to 1 MiB. Each ceiling is above the largest answer those
allow (`edge/filter/forward.go` has the arithmetic; `edge/conformance` checks it
against the pinned chain's genesis), except `block_results` of a block built to be
large, whose public read is cut. The `bulk` class bounds how many such builds run
at once, and `txhash` how many single-tx builds (each bounded by the per-tx cap).

A changed allowlist or cap is a change to `edge/filter/`
here, with a case in its tests, then `bin/build-edge.sh --pin`; it goes onto the lease
with an in-place PUT (`bin/deploy.sh`) like any image change. A chain release that
adds or changes a route a client needs, or changes what gas-check reads, needs this
before it is deployed: bump `edge/conformance/chain.pin`, run the conformance tests,
fix the filter, rebuild.

### The backend's credential

Every call the backend makes is in the public allowlist above; the filter serves the
backend nothing it does not serve everyone, and it never forwards `Authorization`.
What the backend needs is to be exempt from the **per-IP rate limits** (one address
does every user's gas grants and a full re-index from height 1) and not to be starved
by a public flood. So it sends
`Authorization: Basic base64("earth-backend:" + CHAIN_EDGE_TOKEN)` (backend repo
`services/edge.py`, over HTTPS only), and:

- rule 0 skips the rate limiting rules for that exact header, and nothing else: no
  custom rule is skipped, so the websocket block applies;
- the filter gives a request carrying it the backend's reserved slots in each class
  (table above) and its own body budget (R5-E-7). The edge holds only the header's
  SHA-256 (`EDGE_BACKEND_AUTH_SHA256`, which `bin/build-sdl.py` computes from
  `CHAIN_EDGE_TOKEN` in this repo's `.env` and requires with `--tunnel`), never the
  token.

A leaked token buys an address the backend's rate and its few reserved slots (the
per-class caps still bound what reaches the signer, and the public slots are not
affected), not the R3-BD-1 harm it bought under the old allowlist (R4-E-6). The token
is made once by `bin/gen-edge-token.sh`, which writes it into this repo's and the
backend's `.env` without printing it (RELAUNCH.md section 2, before the first
`--tunnel` build). Rotating it is `bin/gen-edge-token.sh --replace` (both `.env`
files), rule 0, then an in-place deploy of both; not while the validator's pod is
not Ready (before `genesis_time`), when the PUT would not be applied.

Compute the header value from the backend's `.env` on the operator's machine:

    printf 'earth-backend:%s' "$CHAIN_EDGE_TOKEN" | base64 | tr -d '\n'

The value sits in the rule's expression, visible to anyone with dashboard access to the
zone.

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

120 requests per 1 minute, then block for 1 minute. The search is one block's txs
(`tx.height=N`, the explorer), and the filter gives it one slot for everyone, so without a per-IP limit one address could keep it busy and
everyone else would get `503`. The path is the decoded one only after the filter has
refused `%`-escapes, so `/cosmos/tx%2Fv1beta1/txs` cannot dodge this rule into
rule 3 and still be served.

**Rule 3. Rate limiting rule, everything else**, characteristic IP:

    (http.host in {"lcd.erth.network" "rpc.erth.network"})

1,200 requests per 1 minute, then block for 1 minute. A flood stop for one address,
not capacity planning: behind carrier NAT one IPv4 address fronts many phones (a
commit poll is a point read; a phone that sends a tx polls at most 20 times), and the
filter's caps are what bound the signer's load. The backend skips it (rule 0).

**Cache rule, `genesis_chunked`** (Caching → Cache Rules, every plan):

    (http.host eq "rpc.erth.network" and http.request.uri.path eq "/genesis_chunked")

Eligible for cache, edge TTL "use cache-control header if present" (the filter sends
`public, max-age=3600, s-maxage=86400` on a `200`). The genesis is ~1.75 MB a call
and immutable for the chain's life, so this takes it off the node. **Purge it on a
relaunch** (RELAUNCH.md §3.4): a new genesis under the same hostname would otherwise
be served stale for up to a day. Nothing else is cached: heights move, and a cached
`/status` would stall the indexer.

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

First the automated check, which also belongs in a schedule (cron, or any alerting
job: it exits 1 on a failure). It asserts the `X-Earth-Edge` header on both hostnames
and that an unknown RPC method, an unknown LCD route, `Simulate` over `abci_query` and
`count_total` come back refused by the filter (R5-E-8). Every probe is cheap for the
node as well, since a misrouted hostname hands the node whatever the check sends, on
every scheduled run (R6-E-5):

    bin/check-edge.py          # --rpc/--lcd to point it elsewhere

Then by hand, from an address that does not hold the token, **only once the automated
check passes**: several of these are scans the node would run in full if a hostname
reached it directly. Expect `403` with `refused by the edge filter` unless noted. A
`200` on any of the refused lines means the hostname reaches the node directly
instead of `edge`: fix the Public Hostname before anything else.

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
    # refused: unbounded costs (R5-E-1..4)
    c "$L/cosmos/tx/v1beta1/txs?query=transfer.recipient%3D%27earth17xpfvakm2amg962yls6f84z3kell8c5lthcx95%27&limit=1"
    c "$L/cosmos/staking/v1beta1/validators?pagination.count_total=true"
    c "$L/cosmos/gov/v1/proposals?pagination.offset=100000"
    j '{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.tx.v1beta1.Service/Simulate","data":"00"}}'
    j '{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.bank.v1beta1.Query/AllBalances","data":"00"}}'
    c "$L/cosmos/tx/v1beta1/txs?query=tx.height%3D1"                                  # 200 (one block)
    curl -sI "$R/status" | grep -i x-earth-edge                                       # x-earth-edge: 1

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
would serve the public with no filter at all. `bin/check-edge.py` ("Check from
outside") catches it; run it after every tunnel or lease change.

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
`node/relayer.sh` instead of the node entrypoint. Co-locating it with the validator
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

**Batch size and gas on earth.** Earth charges for the bytes a tx leaves in
its result (chain `app/result_cap.go`): the first 8 KiB per tx are free, then
20 gas per byte, and one msg over 1 MiB fails its tx (the cap is per msg, so a
few large packets never fail a batch). An ordinary `MsgRecvPacket` is ~3.7 KB;
one whose sender used ibc-go's 32 KiB memo maximum is ~168 KB and ~3.4M gas,
and anyone on the counterparty can send those for the price of one transfer
there. `RELAYER_MAX_MSGS` (default 10, rly's `--max-msgs`) bounds a batch: ten
max-memo packets are ~34M gas, ~51M with the 1.5 gas adjustment, inside the
100M block. Keep the gas adjustment at 1.3 or more (simulation prices the
bytes), do not set rly's `max-gas-amount` (rly then pays for that limit on
every tx), and keep the earth balance funded for bursts: a run of max-memo
packets costs about 0.02 ERTH each at 0.006uerth. Hermes users: set
`max_msg_num = 10` for earth (Hermes' default 30 lets thirty max-memo packets
ask for ~100M gas, a whole block) and `max_gas` to at least 60M.
A packet naming an IBC callback contract on earth cannot push its receive over
the cap: the chain fails a callback that emits more than 256 KiB (an error
acknowledgement, the packet still received).

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
