# Sourced by entrypoint.sh and relayer.sh before they do anything else.
#
# The node runs as `earth`, not root. earthd executes native code on input
# strangers choose — the proof verifier, the CosmWasm engine — and a bug there
# exploited as root is root in the container, with the volume and the
# validator key under it.
#
# Root only long enough to hand $EARTH_HOME (/data) over: a volume made by an earlier,
# root-running image is root-owned, and a node that cannot write its own home
# does not start. Checked on the top directory, so the recursive chown runs
# once, on the first start after upgrading the image, and not on every restart
# over a large data directory. Then the script re-executes itself as `earth`.
#
# Outside a container (node/entrypoint_test.sh, a developer's shell) the
# script is not root and this does nothing.
if [ "$(id -u)" = 0 ] && id earth >/dev/null 2>&1; then
  home="${EARTH_HOME:-/data}"
  mkdir -p "$home"
  if [ "$(stat -c %U "$home")" != earth ]; then
    chown -R earth:earth "$home"
  fi
  export HOME="$home"
  exec setpriv --reuid=earth --regid=earth --init-groups "$0" "$@"
fi
