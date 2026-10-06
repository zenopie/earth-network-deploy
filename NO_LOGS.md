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
   No IP address, address, note or transaction hash is stored with it.
5. **Nothing is exported.** No logs or analytics go to any third party: no Logpush,
   no log drains, no analytics beacons.

## How each service meets it

| Service | Setting | Enforced by |
| --- | --- | --- |
| Validator node | `EARTHD_LOG_LEVEL=*:info,rpc-server:error`. The node sees only the tunnel connector's address, never a client's, and does not log requests at info. `rpc-server` is held at error because at info it logs websocket remote addresses. | `bin/build-sdl.py` refuses `debug`/`trace` and requires `rpc-server:error` |
| Validator and backend cloudflared | `--loglevel info`: connector state, plus one `ERR` line per request the origin failed to answer (time, Cloudflare ray id, ingress rule, origin service; no client IP, path or headers). The ray id is Cloudflare's own request id, so with Cloudflare's records it names the request; on its own it is a timestamp. At debug, cloudflared logs every request's headers, `CF-Connecting-IP` included. | both repos' `build-sdl.py` refuse `debug`/`trace` |
| Backend | uvicorn `--no-access-log --no-proxy-headers`. Gas-grant logs carry only a refusal kind or a coarse error with hex and long numbers removed. | `entrypoint.py`, `routers/gas.py` `_coarse`, `services/ratelimit.py` sweep (on a timer and on every call; `tests/test_ratelimit.py`) |
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
  **Block** action, never **Log**.

## What is still seen by others

The policy covers what Earth records. Some parties see traffic whatever we do:

- **Cloudflare** terminates TLS for every hostname. It sees each client's IP
  address and the full request and response: paths, queries, bodies (registration
  proofs, transactions) and timing. Earth turns off every export and analytics
  feature it can (above), but Cloudflare keeps its own operational data under its
  own policy. That includes aggregate analytics and the **Security Events** log,
  which records requests that a WAF or rate-limit rule matched, with IP and path,
  for the plan's retention period. Earth does not export it.
- **Akash providers** run the containers. A provider can read container memory,
  volumes and stdout. That is why the services write nothing identifying to stdout,
  and why the per-client counters live only in memory and expire.
- **The chain is public.** Every transaction, registration, nullifier and note
  commitment is on chain for anyone to read. Shielding hides amounts, owners and
  links; it does not hide that a transaction happened or when.
- **Your network** (ISP, VPN, Wi-Fi) sees that you connect to Cloudflare. Use a VPN
  or Tor if that matters to you.

## Changing this

A change that logs more than this file allows needs this file changed in the same
commit, and the privacy policy updated before it ships.
