# No-logs policy

Earth's infrastructure keeps no record of who asked for what. This file says exactly
what that means, so the privacy policy and the docs can cite it. It covers the
services Earth runs: the validator (node, RPC, LCD), the backend (indexer, gas
grants, circuits) and the web app. Each is a container on Akash behind a Cloudflare
tunnel.

## The policy

1. **No request logs.** No service writes a log line holding a client IP address, a
   request path or query, a request body, or a user agent.
2. **No identifiers in logs.** No log line holds a passport nullifier, a handle, a
   country, an address, a note commitment or a transaction hash. A log line never
   pairs a client with anything it sent.
3. **Abuse limits stay in memory and expire.** The backend keeps per-client
   counters keyed by IPv4 address or IPv6 prefix, in memory only. A counter is
   dropped two windows after the client's last request (2 hours for `/gas/register`,
   2 minutes for `/privacy` at the defaults), within 10 seconds of that deadline
   whether or not any later request arrives (a background sweep runs every 10 s),
   and on every restart. Counters never
   hold anything else and are never written to disk.
4. **One thing is stored about a registrant.** To pay each passport's gas grant
   once per 30 days, the backend stores `passport nullifier + day + grant kind +
   time` for 31 days. The nullifier is already public on chain in the registration.
   No IP address, address, note or transaction hash is stored with it. It is a
   SQLite table (`STATE_DB`) on the backend's persistent volume, so the backend's
   Akash provider can read it (below). What it adds to the public chain is "this
   passport asked Earth for gas, at this second".
5. **Nothing is exported.** No logs or analytics go to any third party: no Logpush,
   no log drains, no analytics beacons.

## How each service meets it

| Service | Setting | Enforced by |
| --- | --- | --- |
| Validator node | `EARTHD_LOG_LEVEL=*:info,rpc-server:error`. The node sees only the tunnel connector's address, never a client's, and does not log requests at info. `rpc-server` is held at error because at info it logs websocket remote addresses. | `bin/build-sdl.py` refuses `debug`/`trace` and requires `rpc-server:error` |
| Validator and backend cloudflared | `--loglevel info`: connector state, plus one `ERR` line per request the origin failed to answer. Read from cloudflared 2026.9.3's source (`proxy/logger.go`, `proxy/proxy.go`): the line carries the time, `connIndex`, `cfRay`, `ingressRule`, `originService` and `error`. The error is cloudflared's wrapper text ("Unable to reach the origin service…", "Incoming request ended abruptly…") around the error of an `http.Transport.RoundTrip` to the origin (a dial to `node:26657`, a timeout, EOF, a cancelled context). `RoundTrip` errors do not carry the URL (only `http.Client` wraps them with it), so the line has no client IP, path, query or header values. Not yet observed on a live failure; force one on a scratch node before quoting it as measured. The ray id is Cloudflare's own request id, so with Cloudflare's records it names the request; on its own it is a timestamp. At debug, cloudflared logs every request's method, URL and headers, `CF-Connecting-IP` included. | both repos' `build-sdl.py` refuse `debug`/`trace` under every spelling (`--loglevel`, `--transport-loglevel`, `--proto-loglevel`, their `TUNNEL_*` env), and `--logfile`, `--log-directory`, `--trace-output`, `--config` |
| Validator request filter (`edge`, earth-edge) | Logs one line at start (its listen and upstream addresses, route counts) and one on a fatal error, nothing per request: no address, path, query, body or header. Go's HTTP server error log, which can name a remote address, is discarded. It keeps no per-client state (its limits are per cost class, not per client), and it drops `CF-Connecting-IP`, `X-Forwarded-For`, `User-Agent`, `Cookie` and `Authorization` before forwarding, so the node never sees them. The backend's `Authorization` is recognised by its SHA-256 (for reserved capacity) and is not stored or logged either. A refusal is a `403` to the client and nothing anywhere else. Address history is not served at all: the public LCD has no address search, and the validator indexes no address events (wallets list the txs they sent from their own record). | chain repo `docker/edge/main.go` (`ErrorLog: io.Discard`), `filter/forward.go` (`clientHeaders`), `filter/limit.go` (`isBackend`); `filter_test.go` checks the headers |
| Backend | uvicorn `--no-access-log --no-proxy-headers`. Gas-grant logs carry only a refusal kind or a coarse error with hex and long numbers removed. The indexer's chain lease alerts drop the event's address and mask any address in its error text. The rate-limit sweep's error line names only the exception class. | `entrypoint.py`, `routers/gas.py` `_coarse`, `services/privacy/indexer.py` `_log_lease_alerts`, `services/ratelimit.py` sweep (on a timer and on every call; `tests/test_ratelimit.py`, `tests/test_groundworks_leases.py`) |
| Web app | nginx `access_log off`, error log at `crit` | web repo `nginx.conf` |

Peer-to-peer logs (port 26656) can name the addresses of other nodes that connect to
the validator. Those are node operators, not wallet users: wallets never connect over
p2p.

## Cloudflare settings (zone `erth.network`)

Set these before launch, and check them after any dashboard change:

- **Analytics & Logs → Logpush**: no jobs. **Instant Logs**: never started.
- **Web Analytics**: off for every hostname. No automatic JS beacon is injected.
- **Network Error Logging**: off. Otherwise Cloudflare adds `NEL`/`Report-To`
  headers and browsers send it network error reports.
- **Zaraz**: off. **Workers**: none on these hostnames.
- **Zero Trust**: no Gateway logging, and no Access application on these
  hostnames. The deferred remote signer (`akash/REMOTE_SIGNER.md`) would add one for
  operator-only traffic.
- WAF and rate-limit rules (akash/README.md, "Public RPC and LCD limits") use the
  **Block** action, never **Log**. Rule 0 (the backend's Skip) has **Log matching
  requests** unticked.
- **Browser Integrity Check off** (Security → Settings, or a Configuration Rule for
  `lcd.*`, `rpc.*` and `api.*`) and **Security Level** at its lowest ("Essentially
  Off"; where the dashboard offers only "I'm Under Attack", leave that off), and
  **Bot Fight Mode off**. Each challenges or blocks requests on Cloudflare's own
  judgement and writes a Security Event for each, with IP and path. They are on by
  default and would mostly catch the VPN and Tor exits this policy recommends. None
  is needed: the wallets and the backend are not browsers, and the edge filter and
  the rate limits are the protection.
- **DDoS protection cannot be turned off.** Cloudflare's HTTP DDoS managed ruleset is
  always on, on every plan. Its sensitivity can be lowered, not removed; when it
  mitigates, each mitigated request is a Security Event with IP and path.
- **Remote log streaming (`cloudflared tail`).** cloudflared streams its log events to
  Cloudflare's management service on request, at every level including debug,
  whatever `--loglevel` says (`logger/create.go`: the management writer gets every
  event; the level filters only the local output). A debug stream carries each
  request's method, URL and headers, `CF-Connecting-IP` included. Starting one needs
  Cloudflare account access to the tunnel (`cloudflared tail` with an origin
  certificate from `cloudflared login`, a token from the Cloudflare API, or the
  dashboard's live logs), not the connector's `TUNNEL_TOKEN`. Nothing is stored by
  it, but anyone with that access can watch requests live. **We never run it in
  production**; if it is ever needed to debug, it is run against a scratch tunnel.
  Account access is limited to the operators.

## What is still seen by others

The policy covers what Earth records. Some parties see traffic whatever we do:

- **Cloudflare** terminates TLS for every hostname. It sees each client's IP
  address and the full request and response: paths, queries, bodies (registration
  proofs, transactions) and timing. Earth turns off every export and analytics
  feature it can (above), but Cloudflare keeps its own operational data under its
  own policy, visible to anyone with dashboard access to the zone, for the plan's
  retention period. Earth does not export any of it. What remains, with every
  setting above applied:
  - **Security Events**, with IP, path, query and user agent, for every request that
    one of our rules blocks or rate-limits (rule 1, websocket upgrades; rules 2 and
    3, floods) or that Cloudflare's DDoS protection mitigates. Requests the edge
    filter refuses are not among them: Cloudflare passes them and sees only a `403`
    answer, as for any request. One ordinary case lands here: Keplr's own send screen
    opens `rpc.erth.network/websocket`, which rule 1 blocks, so a Keplr-native send
    leaves an event with the IP and that path (not the transaction, which is never
    sent on the socket). With **Network → WebSockets** off instead of rule 1, check
    whether Cloudflare records the refused upgrade before relying on it not to.
  - **Analytics**: aggregate traffic, and sampled individual requests (IP, path,
    country, user agent) whether or not any rule matched them, which the dashboard
    can show.
  - The live stream above, if anyone with account access starts one.
- **Akash providers** run the containers. A provider can read container memory,
  volumes and stdout. That is why the services write nothing identifying to stdout,
  and why the per-client counters live only in memory and expire. Apart from the
  gas-grant record (policy 4), which is on the backend's volume and so readable by
  its provider, our services write nothing identifying to disk or to their logs.
- **The chain is public.** Every transaction, registration, nullifier and note
  commitment is on chain for anyone to read. Shielding hides amounts, owners and
  links; it does not hide that a transaction happened or when.
- **Your network** (ISP, VPN, Wi-Fi) sees that you connect to Cloudflare. Use a VPN
  or Tor if that matters to you.

## Changing this

A change that logs more than this file allows needs this file changed in the same
commit, and the privacy policy updated before it ships.
