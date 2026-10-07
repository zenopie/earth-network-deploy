#!/usr/bin/env bash
# Generate CHAIN_EDGE_TOKEN, the backend's credential at the edge, and write it
# straight into both .env files, printing nothing of it:
#   this repo's .env             build-sdl.py derives EDGE_BACKEND_AUTH_SHA256
#                                (the edge's backend reserve) from it
#   earth-network-backend/.env   the backend sends it (services/edge.py)
# Each .env is first copied to .env.bak-<timestamp> (mode 600).
#
# Run it once, before the first --tunnel build (RELAUNCH.md, section 2,
# prerequisites): the lease's edge holds the hash of the token in force when it
# was created, and a PUT to the validator's pod is not applied until the pod is
# Ready at genesis_time.
#
#   bin/gen-edge-token.sh              generate, unless a token is already set
#   bin/gen-edge-token.sh --replace    rotate: a new token in both files. Then
#                                      rule 0, an in-place deploy of this repo
#                                      (bin/deploy.sh) and of the backend
#                                      (akash/README.md, "The backend's credential")
#
# Already set and equal in both files: nothing to do. Set in one only: copied to
# the other. Set in both but different: refused without --replace.
#
# Paths: DEPLOY_ENV (default: this checkout's .env) and BACKEND_ENV (default
# $PROJECTS_DIR/earth-network-backend/.env, PROJECTS_DIR defaulting to
# ~/Documents/projects).
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
ROOT="${PROJECTS_DIR:-$HOME/Documents/projects}"
DEPLOY_ENV="${DEPLOY_ENV:-$here/.env}"
BACKEND_ENV="${BACKEND_ENV:-$ROOT/earth-network-backend/.env}"

replace=0
case "${1:-}" in
  "") ;;
  --replace) replace=1 ;;
  *) echo "usage: gen-edge-token.sh [--replace]" >&2; exit 2 ;;
esac
for f in "$DEPLOY_ENV" "$BACKEND_ENV"; do
  [ -f "$f" ] || { echo "missing $f (set DEPLOY_ENV / BACKEND_ENV)" >&2; exit 1; }
done

python3 - "$DEPLOY_ENV" "$BACKEND_ENV" "$replace" <<'PY'
import os, re, secrets, shutil, sys, time
deploy, backend, replace = sys.argv[1], sys.argv[2], sys.argv[3] == "1"
VAR = "CHAIN_EDGE_TOKEN"
OK = re.compile(r"[A-Za-z0-9_-]{32,128}")

def current(path):
    val = None
    for line in open(path).read().splitlines():
        m = re.match(r"^\s*%s\s*=(.*)$" % VAR, line)
        if m:
            v = m.group(1).strip()
            if len(v) >= 2 and v[0] == v[-1] and v[0] in "'\"":
                v = v[1:-1]
            val = v or None
    return val

def write(path, tok, stamp):
    bak, n = "%s.bak-%s" % (path, stamp), 1
    while os.path.exists(bak):
        n += 1
        bak = "%s.bak-%s.%d" % (path, stamp, n)
    shutil.copy2(path, bak)
    os.chmod(bak, 0o600)
    out, done = [], False
    for line in open(path).read().splitlines():
        if re.match(r"^\s*%s\s*=" % VAR, line):
            if not done:
                out.append("%s=%s" % (VAR, tok))
                done = True
        else:
            out.append(line)
    if not done:
        out.append("%s=%s" % (VAR, tok))
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        f.write("\n".join(out) + "\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)

d, b = current(deploy), current(backend)
for path, v in ((deploy, d), (backend, b)):
    if v is not None and not OK.fullmatch(v):
        sys.exit("%s: %s is not 32-128 characters of [A-Za-z0-9_-]; fix it or pass --replace" % (path, VAR))
stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())

if replace:
    tok = secrets.token_urlsafe(48)  # 64 characters of [A-Za-z0-9_-]
    write(deploy, tok, stamp)
    write(backend, tok, stamp)
    print("new %s written to both .env files (old copies: .env.bak-%s)." % (VAR, stamp))
    print("Now: rule 0's header value, then an in-place deploy here (bin/deploy.sh) and of the backend.")
elif d and b:
    if d != b:
        sys.exit("%s differs between %s and %s: refusing to choose. --replace generates a new one for both."
                 % (VAR, deploy, backend))
    print("%s already set, the same in both .env files: nothing to do." % VAR)
elif d or b:
    tok, dst = (d, backend) if d else (b, deploy)
    write(dst, tok, stamp)
    print("%s copied into %s (old copy: .env.bak-%s)." % (VAR, dst, stamp))
else:
    tok = secrets.token_urlsafe(48)
    write(deploy, tok, stamp)
    write(backend, tok, stamp)
    print("%s generated and written to both .env files (old copies: .env.bak-%s)." % (VAR, stamp))
PY
