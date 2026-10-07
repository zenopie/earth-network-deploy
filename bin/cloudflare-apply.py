#!/usr/bin/env python3
"""Apply the erth.network Cloudflare rules and settings (akash/README.md,
"Cloudflare rules"; NO_LOGS.md, "Cloudflare settings") through the API.

    bin/cloudflare-apply.py            show what would change, change nothing
    bin/cloudflare-apply.py --apply    apply it

Reads CF_API_TOKEN (zone erth.network: WAF, cache rules, zone settings, DNS)
and CHAIN_EDGE_TOKEN from .env and prints neither. Idempotent: our rules carry
a ref ("earth-…"); rules without one are kept as they are. The rate limits
follow the zone's plan (README "What each plan accepts").
"""
import base64, json, os, sys, urllib.error, urllib.request

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ZONE = "erth.network"
HOSTS = '{"rpc.erth.network" "lcd.erth.network"}'


def env(name):
    for line in open(os.path.join(REPO, ".env")):
        if line.startswith(name + "="):
            v = line.split("=", 1)[1].strip().strip("'\"")
            if v:
                return v
    sys.exit(f"{name} is not set in .env")


TOKEN = env("CF_API_TOKEN")


def cf(method, path, body=None):
    req = urllib.request.Request(
        "https://api.cloudflare.com/client/v4" + path, method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Authorization": "Bearer " + TOKEN, "Content-Type": "application/json"})
    try:
        return json.load(urllib.request.urlopen(req, timeout=60))
    except urllib.error.HTTPError as e:
        return json.load(e)


def ok(r, what):
    if not r.get("success"):
        sys.exit(f"{what}: {r.get('errors')}")
    return r["result"]


zone = ok(cf("GET", f"/zones?name={ZONE}"), "zone")[0]
zid, plan = zone["id"], zone["plan"]["legacy_id"]  # free, pro, business, enterprise
auth = base64.b64encode(("earth-backend:" + env("CHAIN_EDGE_TOKEN")).encode()).decode()

custom = [
    {"ref": "earth-0-backend-skip-ratelimit", "description": "earth 0: backend skips rate limits",
     "expression": f'(http.host in {HOSTS} and http.request.headers["authorization"][0] eq "Basic {auth}")',
     "action": "skip", "action_parameters": {"phases": ["http_ratelimit"]}, "logging": {"enabled": False}},
    {"ref": "earth-1-block-websocket", "description": "earth 1: no websockets to rpc/lcd",
     "expression": f'(http.host in {HOSTS} and (http.request.uri.path eq "/websocket" '
                   'or any(lower(http.request.headers.names[*])[*] eq "upgrade")))',
     "action": "block"},
]
if plan == "free":
    paths = ('(starts_with(http.request.uri.path, "/cosmos/") or starts_with(http.request.uri.path, "/earth/") '
             'or http.request.uri.path in {"/" "/status" "/block" "/blockchain" "/block_results" "/abci_query" '
             '"/genesis_chunked" "/commit" "/validators" "/consensus_params" "/tx"})')
    ratelimit = [{"ref": "earth-3-rpc-lcd-flood", "description": "earth 3: rpc/lcd per-IP flood stop",
                  "expression": paths, "action": "block",
                  "ratelimit": {"characteristics": ["ip.src", "cf.colo.id"], "period": 10,
                                "requests_per_period": 200, "mitigation_timeout": 10}}]
else:
    search = ('(http.host eq "lcd.erth.network" and http.request.uri.path eq "/cosmos/tx/v1beta1/txs" '
              'and http.request.uri.query ne "")')
    ratelimit = [
        {"ref": "earth-2-tx-search", "description": "earth 2: tx search per IP", "expression": search,
         "action": "block", "ratelimit": {"characteristics": ["ip.src", "cf.colo.id"], "period": 60,
                                          "requests_per_period": 120, "mitigation_timeout": 60}},
        {"ref": "earth-3-rpc-lcd-flood", "description": "earth 3: rpc/lcd per-IP flood stop",
         "expression": f"(http.host in {HOSTS})", "action": "block",
         "ratelimit": {"characteristics": ["ip.src", "cf.colo.id"], "period": 60,
                       "requests_per_period": 1200, "mitigation_timeout": 60}},
    ]
cache = [{"ref": "earth-genesis-chunked", "description": "earth: cache genesis_chunked",
          "expression": '(http.host eq "rpc.erth.network" and http.request.uri.path eq "/genesis_chunked")',
          "action": "set_cache_settings",
          "action_parameters": {"cache": True, "edge_ttl": {"mode": "respect_origin"}}}]

# NO_LOGS.md: these write Security Events with IP and path for ordinary requests.
settings = {"browser_check": "off", "security_level": "essentially_off", "pseudo_ipv4": "off"}

apply = "--apply" in sys.argv[1:]
changed = False
for phase, ours in (("http_request_firewall_custom", custom), ("http_ratelimit", ratelimit),
                    ("http_request_cache_settings", cache)):
    r = cf("GET", f"/zones/{zid}/rulesets/phases/{phase}/entrypoint")
    existing = r["result"]["rules"] if r.get("success") else []
    keep = [x for x in existing if not str(x.get("ref", "")).startswith("earth-")]
    have = {x.get("ref"): x for x in existing if str(x.get("ref", "")).startswith("earth-")}
    same = len(have) == len(ours) and all(
        have.get(o["ref"], {}).get("expression") == o["expression"]
        and have.get(o["ref"], {}).get("action") == o["action"] for o in ours)
    print(f"{phase}: {'up to date' if same else 'will set'} {[o['ref'] for o in ours]}"
          + (f" (keeping {len(keep)} other rules)" if keep else ""))
    if not same:
        changed = True
        if apply:
            # The rule refs carry our identity; the token never reaches stdout.
            ok(cf("PUT", f"/zones/{zid}/rulesets/phases/{phase}/entrypoint",
                  {"rules": [{k: v for k, v in x.items() if k not in ("id", "version", "last_updated")}
                             for x in keep] + ours}), phase)
for name, value in settings.items():
    cur = ok(cf("GET", f"/zones/{zid}/settings/{name}"), name)["value"]
    print(f"{name}: {cur}" + ("" if cur == value else f" -> {value}"))
    if cur != value:
        changed = True
        if apply:
            ok(cf("PATCH", f"/zones/{zid}/settings/{name}", {"value": value}), name)
print(f"plan {plan}; " + ("applied" if apply and changed else "nothing to change" if not changed
                          else "dry run: --apply to change"))
