#!/usr/bin/env python3
# Run ONE command inside a lease container and print its output.
#
#   bin/lease-shell.py --service node -- df -h /data
#
# Why this exists: lease-logs.py answered "what is the node saying", and that was
# enough until the node started degrading for a reason no log line states. Disk
# usage, file counts and directory sizes are facts only the container holds.
#
# The provider's shell endpoint is the same websocket family as logs and events
# (see lease-logs.py for the four things that route encodes), with two
# differences: the token needs the `shell` scope, and the frames are k8s exec
# streams — a leading byte says which stream the rest of the payload belongs to.
#
# UNLIKE lease-logs.py, a `shell` token can change the container. Everything here
# is read-only by intent, not by enforcement: the command is whatever you pass.
# Prefer df/du/ls/cat. The token still cannot touch the deployment itself.
import argparse, json, os, sys, importlib.util

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("leaselogs", os.path.join(HERE, "lease-logs.py"))
ll = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ll)

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--service", default="node")
    ap.add_argument("--dseq", help="lease to target; defaults to DSEQ in .env")
    ap.add_argument("--pod", type=int, default=0)
    ap.add_argument("cmd", nargs=argparse.REMAINDER,
                    help="command to run, after a bare --")
    args = ap.parse_args()

    cmd = [c for c in args.cmd if c != "--"]
    if not cmd:
        sys.exit("give a command, e.g.  --service node -- df -h /data")

    env = ll.load_env()
    dseq = args.dseq or env["DSEQ"]
    # This token can exec into the container: it lives ll.TTL_SECONDS, and
    # ws_stream verifies the provider before sending it.
    token = ll.mint_token(env["AKASH_API_KEY"], ["status", "logs", "events", "shell"])
    host, port, provider = ll.provider_host(env["AKASH_API_KEY"], dseq)

    q = [f"stdin=0", "tty=0", f"podIndex={args.pod}", f"service={args.service}"]
    q += [f"cmd{i}={ll.urllib.parse.quote(c)}" for i, c in enumerate(cmd)]
    path = f"/lease/{dseq}/1/1/shell?" + "&".join(q)

    def emit(frame):
        # Each websocket frame is one type character followed by its payload:
        # 'd' for output, 'f' for the JSON exit status. Learned by dumping
        # frames — the k8s exec stream-id byte never reaches this layer.
        #
        # Split per frame, never on a byte inside the payload: a stray split
        # would corrupt the very thing this tool exists to read accurately.
        if not frame:
            return
        kind, body = frame[0], frame[1:]
        if kind == "f":
            try:
                code = json.loads(body).get("exit_code")
            except json.JSONDecodeError:
                sys.stderr.write(body)
                return
            if code:
                sys.stderr.write(f"[exit {code}]\n")
            return
        sys.stdout.write(body)
        sys.stdout.flush()

    ll.ws_stream(host, port, path, token, emit, provider)


if __name__ == "__main__":
    main()
