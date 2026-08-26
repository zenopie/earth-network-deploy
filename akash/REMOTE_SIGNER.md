# Remote signer

The consensus key currently lives on the Akash node, at
`/data/config/priv_validator_key.json`. Whoever operates that machine can read
it, and with it double-sign — 5% of stake and a permanent tombstone under this
chain's slashing params.

A remote signer splits the two: the node keeps running on rented hardware, the
key moves to a machine you own. The node asks for signatures; it can no longer
make them.

    home (dynamic IP, no open ports)              Akash
      tmkms ──dials──► cloudflared ──tunnel──►  earthd :26659
       key                                       no key

## Why a dynamic IP is fine

tmkms is the client. CometBFT *listens* on `priv_validator_laddr` and the signer
dials in, so the machine holding the key never needs to be reachable. Both
processes at home make outbound connections only: no port forwarding, no static
address, nothing for a router to be configured about.

Cloudflare supplies the stable hostname on the validator's side, which is the
side that actually needs one.

## Second reason to do it, and the one people underrate

tmkms keeps a state file of the last (height, round, step) it signed and refuses
to sign anything at or below it. That is protection against *equivocation*, not
just theft: if the Akash node is compromised, or you accidentally run a second
validator during a migration, the signer will not produce the conflicting
signature. It cannot sign the thing that gets you slashed.

That state file must be durable and monotonic. Losing it is how a remote signer
causes the exact fault it exists to prevent — which is also why an enclave with
ephemeral storage is a poor place to run one.

## Chain side

`PRIV_VALIDATOR_LADDR` in the SDL's env block turns it on:

    env:
      - PRIV_VALIDATOR_LADDR=tcp://0.0.0.0:26659

The entrypoint writes it into config.toml. Unset, nothing changes and the node
signs locally as it does today.

Then expose 26659 to the tunnel service only — never `global: true`:

    expose:
      - port: 26659
        to:
          - service: cloudflared

That keeps the socket off a provider port, so there is no `IP:port` for a
scanner to find. It is necessary and it is nowhere near sufficient — it removes
the direct route, not the reachability. How the signer then reaches it is the
decision that matters; see "Two ways to route it" below.

## What the privval socket does not protect

The connection is Secret Connection encrypted, but on this socket it is
effectively *unauthenticated in both directions*.

tmkms can pin the node's identity — `tcp://<node_id>@host:port` — but there is
nothing stable to pin, because CometBFT mints a throwaway key for the privval
listener on every process start (`privval/listener.go`):

    case "tcp":
      // TODO: persist this key so external signer can actually authenticate us
      listener = NewTCPListener(ln, ed25519.GenPrivKey())

Pin it anyway and the node dies on its next restart: tmkms rejects with
`validator peer ID mismatch`, the node gets `can't get pubkey: send: EOF` and
exits. So the address is configured without the `<node_id>@` prefix, and tmkms
logs `unverified validator peer ID!` on every connect. That warning is expected
and cannot currently be cleared.

The node does not authenticate the KMS either — its listener accepts whoever
connects first.

The consequence worth internalising: **anyone who can reach 26659 can
impersonate the node to the KMS and ask it to sign votes.** They cannot steal
the key, but they can request signatures at heights the real node has not
reached — which is a double-sign/slashing risk, not merely a nuisance. The
double-sign guard in `state/earth-consensus.json` blocks conflicting votes at or
below the heights it has already seen, but it cannot tell a forged future height
from a real one.

Authentication therefore has to come from the transport, and `to: service:
cloudflared` is not it — that only removes the direct route. Do not shortcut it
with a plain `global: true` port even temporarily, and do not mistake it for the
control that keeps strangers out. That control is the Access policy in
the next section. Without one attached, the socket is open to whoever finds the
hostname.

## Routing it: public hostname + Access service token

Add a Public Hostname of type **TCP**:

    signer.erth.network -> tcp://node:26659

Note that this addresses the node by *name*. cloudflared resolves `node` through
the lease's internal service DNS, so the pod's address changing across redeploys
costs nothing — which is the reason this shape and not a private CIDR route.
Routing a private network to a pod means pinning an address the provider
assigns and may recycle, and a stale route points at another tenant.

A Public Hostname is exactly that: public. It is not a private address and the
tunnel is not an authenticator — `cloudflared access tcp` is an ordinary client
and anyone may point one at that name. What keeps them out is the Access policy,
and because both ends are machines it wants a **service token**, not an identity
provider login:

1. Zero Trust -> Access -> Service Auth -> create a service token. Cloudflare
   shows the Client Secret **once**; it cannot be retrieved later, only replaced.
2. Zero Trust -> Access -> Applications -> add a **Self-hosted** application for
   `signer.erth.network`.
3. One policy, action **Service Auth** — not Allow, which expects a human to
   authenticate to an IdP and will lock out a headless signer — with an include
   rule of `Service Token` -> the token from step 1.

Home side:

    cloudflared access tcp --hostname signer.erth.network --url localhost:26659 \
      --service-token-id "$CF_ACCESS_CLIENT_ID" \
      --service-token-secret "$CF_ACCESS_CLIENT_SECRET"
    tmkms start -c tmkms.toml        # addr = tcp://127.0.0.1:26659

`earth-tmkms/new-secrets.sh` writes those values to its gitignored `secrets/`.

Then verify the policy is on. From a machine with no token this must be refused:

    cloudflared access tcp --hostname signer.erth.network --url localhost:26699

Do not skip that. An application that was never attached to the hostname looks
identical to a working one from the authenticated side, and the failure is
silent in the direction that matters.

Layering `Require -> Gateway` or device posture on top is possible if the signer
also runs WARP, but think before adding it: this policy sits in front of a
socket that fails closed. Every additional requirement is one more component
whose outage produces a validator signing nothing.

### Rotating the token

Two-sided, in this order: issue the new token, add it to the policy, restart the
home-side `cloudflared`, then revoke the old one. Revoking first drops the
signer, and a dropped signer is a validator producing no blocks.

## Migrating a live validator

Do this before anyone else bonds stake. Moving a consensus key on a running
validator is the single operation most likely to produce an accidental
double-sign, and the window where both the old node and the new signer believe
they may sign is exactly the fault being guarded against.

1. Stop the validator. Confirm it is not producing blocks.
2. Copy `priv_validator_key.json` to the signer host; import it into tmkms.
3. Seed tmkms's state from `priv_validator_state.json` so it does not start
   believing it has signed nothing.
4. Start tmkms, then the node with `PRIV_VALIDATOR_LADDR` set.
5. Confirm blocks are being signed again.
6. **Delete the key from the Akash host.** Skipping this leaves the key on the
   machine the whole exercise was about getting it off.
