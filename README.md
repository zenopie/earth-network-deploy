# earth-network-deploy

Operator deployment for the earth chain. Private, because it holds the Akash
SDL, the secrets layout, and the runbooks for a node that has a validator key
and a tunnel token behind it.

The chain itself is public at `zenopie/earth-network-chain` — node software,
genesis, Dockerfile, and the entrypoint that lets anyone run a node. Nothing
here is needed to *join* the network; it is only needed to operate this
particular deployment.

    akash/deploy.yaml       the deployed unit (node, cloudflared, relayer)
    akash/README.md         how the lease behaves, and what destroys it
    akash/REMOTE_SIGNER.md  moving the consensus key behind tmkms
    bin/digest.sh           resolve a released tag to its image digest
    bin/build-sdl.py        SDL + digest + secrets -> the copy that gets sent
    bin/deploy.sh           the whole thing, in one command
    docker-compose.yaml     the same image, for a plain Docker host

## Deploying

    bin/deploy.sh v0.4.5             update the lease in place
    bin/deploy.sh v0.4.5 --print     build the SDL and read it, send nothing

In place keeps the volumes, so the chain keeps its height and history. Image and
env changes can go this way; endpoint kinds and resources cannot, because they
are part of what the provider bid on — those need a close-and-recreate, which
**destroys the chain's state**.

## Why the digest is resolved rather than committed

The chain repo used to rewrite this SDL with the image digest and commit it back
to master. That put deployment state in a public repository, and a build racing
a human push meant one of them lost.

So the digest is now read from the registry at deploy time. `bin/digest.sh`
takes a tag and returns `ghcr.io/...@sha256:...`, using no credentials — the
package has to be public anyway or the Akash provider could not pull it. The
public repo no longer knows this file exists.

## Secrets

`.env`, gitignored, shape in `.env.example`. They are injected into the
submitted copy of the SDL, never into the file on disk.

Everything submitted reaches the provider regardless — that is what submitting
means. What the injection avoids is the secrets reaching a repository.

The `AKASH_API_KEY` is the dangerous one: it can close the lease, and closing
destroys the volumes that hold the chain.
