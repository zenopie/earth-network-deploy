# Relaunch runbook: earth-1 from a fresh genesis

The privacy chain (shielded ERTH and ANML as Orchard-style notes, handles, private
personhood, private staking with one stake note per validator) launches from a new
genesis. No state migrates and there are no upgrade handlers. This is the one runbook
for it: the order of operations, the check at each step, and what to do when a step
fails.

Run the steps in order. Each **Check** must pass before the next step.

## Where things stand

| | |
| --- | --- |
| Chain id | `earth-1`, kept. Nothing from any earlier earth-1 carries over. |
| Chain | Nothing running. The last earth-1 lease (`1790678074849`) closed on 2026-10-01, and the v1.0.0 staging lease (`1790917587362`) closed before its genesis on 2026-10-02. v1.0.0 never ran. |
| Validator operator account | `earth1n6amvkgfrrgy6ulhurewnm0endkgye69fkcapr`, made 2026-10-04 by `bin/rotate-launch-keys.sh` (do not re-run it: it replaces this key). Its mnemonic is `VALIDATOR_MNEMONIC` in this repo's `.env`. It replaces `earth14e6sqtf5y7mtzwykqreewe9kg3w94t0f25d54a`. |
| Consensus key | `PRIV_VALIDATOR_KEY_B64` in `.env`, pubkey `PGqvPN4CxEkxvvh3tSBX0SGeBgjMdqQwZkdHt8FRLm4=`. Generated 2026-10-02 and never signed a block, so it is safe to use. The old chain's key (`90603989…`) must never sign again: its public signatures at the same heights under the same chain id would be double-sign evidence. |
| Node key | `NODE_KEY_B64` in `.env`; fixes the node id joiners dial. |
| Gas wallet | `earth13ysugyz4la7kfrmgsfdhk203ujt0jgcpw2avmg`. Its mnemonic is `GAS_WALLET_MNEMONIC` in the backend repo's `.env`. It is not in genesis; the validator funds it after launch (section 6). |
| Backend | v3.0.0 on lease `1790918719150` (provider `akash1aaul837r7en7hpk9wv2svg8u78fdq0t2j2e82z`), idle. Replaced in section 4. |
| Web | v2.0.0 on lease `1787052820013`, served at `erth.network`. Redeployed in section 5. |
| `akash/genesis.sha256` | Pins `acb96128…`, the chain repo's `networks/genesis.json` at `6d3500a`, a **pre-ceremony** genesis that is already stale: the chain repo's genesis is `455d5aae…` at `678c1f9` (4 MiB blocks), and it will change again with every chain commit that touches genesis inputs until the ceremony. So `bin/check-genesis.sh --chain` FAILS the pin now, with or without `--allow-placeholder`; that is expected. Nothing is pinned until the ceremony's genesis exists: section 2, step 4 writes its sha256 here, and from then on the pin and the launch tag's `genesis.json` must match. |
| Launch identities | `launch/launch.json`: the operator, its consensus pubkey, the placeholder accounts the ceremony removes and the consensus key never to reuse. The chain repo holds none of them: its `scripts/ceremony.sh` takes this file (`--launch`, via `bin/ceremony.sh`), and `bin/check-genesis.sh` and `bin/build-sdl.py` read it. |
| Node image | Not built. The chain image is generic (earthd, the release genesis, a minimal entrypoint); our runtime (keys from `.env`, the keyless guard, refusing a foreign genesis, cosmovisor, the relayer) is `node/` here, an image built `FROM` the chain release's image by digest (`node/base.pin`). `akash/deploy.yaml` carries the all-zero placeholder for `ghcr.io/zenopie/earth-network-node` on `node` and `relayer`, refused by every release build; section 2, step 4 builds and pins it (gate 1.5). |
| Edge image | Not built. `akash/deploy.yaml` carries the all-zero placeholder digest for `ghcr.io/zenopie/earth-network-edge`, which every build that pins the node refuses; section 1.4 builds, pushes and pins it. `edge/conformance/chain.pin` names chain `678c1f9` (privacy/orchard, not yet pushed: fetch with `EARTH_CHAIN_SRC`). |

**One node.** The validator is also the full-history node: it serves
`rpc.erth.network` and `lcd.erth.network` through its tunnel, and the privacy indexer
reads `block_results` from it from height 1. There is no separate archive lease.

## 1. Gates: nothing goes on a lease until these exist

1. **The final audit is closed** on every repo's final code (no Medium or higher open).
2. **Wallet release branches ready, final circuits frozen.** Registration, claims,
   votes and every private tx need the final circuits, tx formats and chain id. An old
   build can do none of them on the new chain. The **store builds are not made here**:
   both wallets pin the served genesis hash (section 2, step 5), which exists only
   after the ceremony fixes `genesis_time`. So they are built in section 2, step 6,
   and `genesis_time` is chosen to leave room for store review (section 2 intro).
   What must be ready now: the release branches build, the circuits in them are the
   final ones (their verifying keys are the ones the genesis will carry), and a
   pre-release build of each passes on a devnet. The build steps, for section 2,
   step 6:
   - **iOS (TestFlight, then App Store):** archive from the mobile release branch after the final
     circuit commit; upload with `xcodebuild -exportArchive` (`destination=upload`,
     team `XD8VH8WKVX`, bundle `network.erth.EarthWallet`). iOS bundles the circuits
     from the Android assets at build time: hash the circuits inside the built `.app`
     against the repo copies. A stale bundle fails registration at the proof-binding
     check, and the error looks like an iOS bug. Set `usesNonExemptEncryption=false`
     on the build (App Store Connect API, `PATCH /v1/builds/{id}`) or it stalls at
     MISSING_EXPORT_COMPLIANCE. The Internal group sees every build.
   - **Android:** `./gradlew :app:bundleRelease` from `android/`, uploaded to Play.
     `public/.well-known/assetlinks.json` in the web app must carry the Play
     app-signing fingerprint for `/ref/<handle>` App Links.
   - The verifying keys in the genesis must be the ones these builds prove against.
   - Both builds need the served-genesis pin (`EARTH_GENESIS_SHA256` /
     `genesisSHA256`, section 2, step 5). A build without it refuses every node in
     the own-node setting as "other genesis".
3. **Chain release candidate**: everything merged, `make genesis-check` and the test
   suite green. The tag is cut at the ceremony (section 2), because the genesis is
   baked into the image.
4. **The edge filter checked against that chain, built and pinned.** earth-edge lives
   in this repo (`edge/`) and ships as its own image, not in the node's:
   - Point `edge/conformance/chain.pin` at the release candidate's commit (the launch
     tag's, once cut; the protos must be the ones the node serves), then

         edge/conformance/fetch-chain.sh       # EARTH_CHAIN_SRC=<chain clone> before it is pushed
         (cd edge && go vet ./... && go test ./...)
         (cd edge/conformance && CI=1 go test ./...)

     all green, `CI=1` so nothing skips. A conformance failure is the filter's
     allowlist drifting from the chain's routes or versions: fix `edge/` in the same
     commit as the pin.
   - `docker login ghcr.io` (a token with `write:packages`), then
     `bin/build-edge.sh --pin`: it builds `edge/Dockerfile` for linux/amd64 from the
     committed `edge/` (the filter's tests run in the build), pushes
     `ghcr.io/zenopie/earth-network-edge:<commit>`, checks that an anonymous pull
     resolves to the digest it pushed (the package must be **public**, or the
     provider cannot pull it: GitHub → Packages → earth-network-edge → settings →
     visibility), and writes `…@sha256:<digest>` into the `edge` service of
     `akash/deploy.yaml`. Commit that line. `build-sdl.py` refuses the all-zero
     placeholder, a tag, or a `command` on `edge`.
   - Any later change to `edge/` **or to `chain.pin`** repeats both steps:
     conformance against the pinned chain, then `bin/build-edge.sh --pin`.
     Conformance is the build gate, so the pinned edge digest must come from a commit
     whose conformance passed against the chain it will front. A change to
     `chain.pin` alone usually leaves the image bytes the same (`edge/conformance/`
     is outside the build context), and then `--pin` writes the same digest and there
     is nothing to commit. Rebuild anyway; do not reason about whether the bytes
     changed. A new digest goes out with `bin/deploy.sh` like any SDL change (an image
     change on `edge` only; the node's volume is untouched).
   - `edge/conformance` also checks the answer ceilings (`edge/filter/forward.go`)
     against the pinned chain's genesis (block `max_bytes`, `max_gas`, evidence) and
     its result caps, read from the chain's `app/resultcap` source: a chain release
     that changes, renames or removes one fails here until
     `edge/filter/chaincaps.go` and the ceilings are re-derived.
5. **The node image built on the launch tag and pinned.** The chain's image is
   generic; `node/` is ours (`node/Dockerfile` `FROM` the chain image by digest,
   `node/entrypoint.sh`, `node/relayer.sh`, `node/drop-root.sh`, cosmovisor and rly
   built in). It needs the launch tag's image, so it is done in section 2, step 4:

       node/entrypoint_test.sh                    # all pass
       bin/build-node.sh --base <launch tag>      # writes node/base.pin; commit it
       bin/build-node.sh --pin                    # builds, pushes, pins node + relayer

   `--pin` writes `ghcr.io/zenopie/earth-network-node@sha256:<digest>  # FROM <base>`
   on both image lines; commit them, and make the `earth-network-node` package
   **public**. The build passes `akash/genesis.sha256` too: it fails unless the base
   image's baked genesis hashes to it, and labels the image with the base and that
   sha256. `bin/check-node-image.py` reads those labels back from the registry, after
   the push and in every `deploy.sh` / `create.sh` before anything is sent, and
   checks that the base image's layers (by content, `rootfs.diff_ids`) are the node
   image's first layers, so neither the `# FROM` comment nor a label can claim a base
   the image was not built on. So the genesis
   pin is written (section 2, step 4) before the node image is built. `build-sdl.py` refuses a release build (`deploy.sh` / `create.sh
   <tag>`) unless the tag's chain image is `node/base.pin` and both lines' `FROM`,
   and refuses the placeholder, the chain image itself, or a `command` on `node`.
   Any later change to `node/` repeats `--pin` (no new `--base`).
6. **Backend release** with `EARTHD_VERSION` / `EARTHD_SHA256` (backend `Dockerfile`)
   bumped to the launch tag. `/gas/register` runs `earthd gas-check registration`,
   the chain's own check: an older binary cannot check the new formats. So this
   release follows the chain tag (section 4.1).
7. **Web app release** from its release branch: `npm run build` and `npm run check`
   pass (`check:dex-live` needs the chain; run it after launch).
8. **Docs**: the docs release branch, with the `TODO(relaunch)` placeholders in
   `docs/run-a-node/join.md` (launch tag, genesis sha256, node id, P2P address) filled
   in once sections 2 and 3 have the values. Published at launch.

## 2. Genesis ceremony (chain repo)

Pick **`genesis_time`** at the ceremony, not before: a real UTC instant when you have
hours free to watch the launch, and **at least 7 days after the ceremony**. The
wallets' store builds can only be made after the ceremony (step 5 pins the served
genesis hash, which depends on `genesis_time`), and they must be live before it:
App Store review usually takes 1-2 days but a rejection restarts it, and Google Play
review of a production release can take several days (up to 7). Seven days covers
one round of each with a resubmission; take 10 if either store has been slow
lately. Emission and the POL burn are prorated from `genesis_time`, so a time
already past pays the whole gap out at height 2.

**Prerequisite: the backend's edge credential**, once, before anything below. Every
`--tunnel` build (step 1, section 3) derives the edge's backend reserve
(`EDGE_BACKEND_AUTH_SHA256`) from `CHAIN_EDGE_TOKEN` in this repo's `.env`, and the
backend sends the same token (section 4). Generate it straight into both `.env`
files, printing nothing:

    bin/gen-edge-token.sh        # this repo's .env and earth-network-backend/.env

It does nothing if both already hold the same token, copies it if only one does, and
refuses two different ones. Never regenerate it after section 3, step 2: the
lease's edge holds the hash of the token it was created with, and a PUT to the
validator's pod is not applied until the pod is Ready at `genesis_time`, so the
backend would have no reserve until after launch. (Rotation later is
`bin/gen-edge-token.sh --replace`, then rule 0 and an in-place deploy of both;
akash/README.md, "The backend's credential".)

1. **Read the key facts off the `.env`**, sending nothing (no tag needed; the image
   line stays the committed placeholder):

       python3 bin/build-sdl.py . /dev/null --fullnode --no-statesync \
         --validator-key --node-key --tunnel

   It prints the `node id` (from `NODE_KEY_B64`) and the consensus address and pubkey
   (from `PRIV_VALIDATOR_KEY_B64`). It refuses a key whose pubkey is not
   `launch/launch.json`'s `consensus_pubkey` (`PGqvPN4C…`), one in its
   `used_consensus_keys` (the old chain's `kTMzo…`), or an address that does not
   derive from the pubkey. With `--tunnel` it also needs `CHAIN_EDGE_TOKEN` in `.env`
   (the prerequisite above): the edge's backend reserve is derived from it.
2. **Run the ceremony** on the operator's machine, from this repo, against the chain
   checkout at the release candidate:

       bin/ceremony.sh <chain checkout> --genesis-time <RFC3339, UTC> \
         --memo-peer <node id>@<host>:26656 --moniker <name>

   That runs the chain repo's `scripts/ceremony.sh --launch launch/launch.json
   --env-file .env …`: the chain repo holds no launch identity, this repo's
   `launch/launch.json` is them. It reads only the `VALIDATOR_MNEMONIC` line of `.env`,
   never prints it, and refuses a mnemonic that is not `launch.json`'s `operator`
   (`earth1n6amvkgfrrgy6ulhurewnm0endkgye69fkcapr`). All or nothing, it:
   - swaps the placeholder validator (the committed gentx's signer,
     `earth14e6sqtf5y7mtzwykqreewe9kg3w94t0f25d54a`) for the operator (same 1,000
     ERTH) in `networks/genesis/accounts.json`, and removes `remove_accounts` (the
     devnet faucet `earth1s7rgs…` and the old gas wallet `earth1jtc2z…`);
   - writes `genesis_time` (refused unless in the future) to `chain.json`;
   - signs a new gentx with `consensus_pubkey` (no private consensus key is
     written), keeping the placeholder's self-delegation (100 ERTH) and commission. A
     key in `used_consensus_keys` (`kTMzo…`, consensus address `90603989…`) is
     refused;
   - rebuilds the genesis (`scripts/build-genesis.sh`, then `--check`) and runs the
     genesis tests with `EARTH_REQUIRE_CEREMONY=1 EARTH_CEREMONY_CONFIG=launch.json`,
     then prints the sha256.

   `--memo-peer` and `--moniker` are required (no defaults). `ceremony.sh` refuses a
   private, loopback, link-local or unspecified host and the placeholder gentx's
   moniker (`earth-akash-devnet`).

   **The memo's port will be wrong, permanently.** The memo is sha-pinned into the
   launch genesis, and the ceremony runs before the lease exists. Akash maps the
   container's 26656 to a port the provider picks when the lease is created (section
   3, step 3, reads it from the lease status), and it changes with every new lease, so
   no value written now can be right. Nothing reads the memo to connect: CometBFT
   dials `persistent_peers`/`seeds` from config, and joiners get the real address from
   the docs' P2P address (section 6.6), which is filled in after the lease. Use:

       --memo-peer <node id from step 1>@p2p.erth.network:26656

   The node id is right forever (`NODE_KEY_B64`). The host is a name you control: after
   the lease, point `p2p.erth.network` (an A record, **DNS only**, not proxied:
   Cloudflare's proxy does not carry CometBFT's TCP) at the provider's ingress address
   if you want it to resolve; the port in the memo stays `26656` and is not the one to
   dial. Say so in the join docs (section 6.6).
3. Confirm in `networks/genesis.json`:
   - `chain_id` `earth-1`, `genesis_time` as chosen, one gentx: operator
     `earthvaloper1…` of `earth1n6amv…`, pubkey `PGqvPN4C…`;
   - no balance but the validator's and the module accounts';
   - genesis account numbers start at the large offset (old earth-1 txs cannot
     replay);
   - every verifying key (register, action, membership, stake, vote) matches the
     store builds.
4. Commit the sources and the rebuilt genesis. **Tag the launch release** from that
   commit: binaries (`linux/amd64`, `linux/arm64`), `checksums.txt`, `genesis.json`, and
   the image on ghcr. The image's baked `/etc/earth/genesis.json` is what every node
   installs. **In this repo**, write the printed sha256 into `akash/genesis.sha256`
   and commit. Then build and pin the node image on that tag (gate 1.5):
   `bin/build-node.sh --base <tag>`, commit `node/base.pin`, `bin/build-node.sh
   --pin`, commit `akash/deploy.yaml` (the build needs the committed genesis pin:
   it checks the base's genesis against it); and point `edge/conformance/chain.pin`
   at the tag's commit, rerun conformance and rebuild and re-pin the edge image
   (`bin/build-edge.sh --pin`, gate 1.4; commit if the digest changed).
5. **Pin the wallets' genesis check.** Earth Wallet's own-node setting compares a
   node's `/genesis_chunked` with a pinned hash. CometBFT serves its own
   re-encoding of the genesis, not the file bytes, so this hash differs from
   `akash/genesis.sha256`. In the mobile repo run
   `go run ./tools/genesishash <the tag's genesis.json>`. It prints both hashes;
   check the file hash equals `akash/genesis.sha256`, then write the **served**
   hash into Android's `Constants.EARTH_GENESIS_SHA256` (`Constants.kt`) and iOS's
   `Constants.genesisSHA256` (EarthCore `Constants.swift`), and into
   `akash/genesis-served.sha256` here. After the validator is up, confirm
   against the live node. The pinned hash is over the base64-decoded
   `result.data` of every chunk, concatenated in order (the pipeline in
   `tools/genesishash`'s header), not over the JSON response:

       curl -s 'https://rpc.erth.network/genesis_chunked?chunk=0' | jq -r .result.total   # 1 at 1.3 MB
       for i in $(seq 0 $(( $(curl -s 'https://rpc.erth.network/genesis_chunked?chunk=0' | jq -r .result.total) - 1 ))); do
         curl -s "https://rpc.erth.network/genesis_chunked?chunk=$i" | jq -r .result.data | base64 -d
       done | shasum -a 256

   It must print the served hash.
6. **Store builds of both wallets**, now, with the pin from step 5 (gate 1.2 has the
   steps): iOS archived and uploaded to TestFlight, then submitted to App Store review;
   Android `bundleRelease` uploaded to Play and submitted for review. Both must be
   **live before `genesis_time`**: that is what the 7-day margin above is for. Before
   submitting, check each build carries the step 5 served hash and the final circuits
   (hash the circuits inside the built `.app` against the repo copies). If review runs
   out the clock, `genesis_time` cannot move without a new ceremony (it is in the
   genesis): either launch with the apps still in review (nobody can register or
   transact from a store build until they are live) or redo the ceremony with a later
   time, which changes the genesis sha and both pins.

**Check:**

    bin/check-genesis.sh <tag> --chain <chain checkout at the tag>   # no FAIL, no --allow-placeholder
    bin/digest.sh <tag>                                              # resolves to ghcr...@sha256, = node/base.pin
    python3 bin/build-sdl.py . /dev/null "$(bin/digest.sh <tag>)" --fullnode --no-statesync   # no refusal

and, in a container from the node image, `earthd version --long` shows the tag's
commit.

## 3. Validator lease

A **new lease** with a new 200Gi volume. Nothing is parked: no old node is running.
Before creating it, the Cloudflare dashboard must show **no** active connector on the
chain tunnel (one token, one live connector).

1. **Dry run**, sending nothing:

       bin/create.sh <tag> --fullnode --no-statesync --validator-key --node-key \
         --tunnel --first-validator-lease --sdl-only

   Add `--genesis <the tag's release genesis.json>` to `build-sdl.py` (same flags, `.`
   and `/dev/null`) once: it checks that file hashes to `akash/genesis.sha256` and that
   its gentx names this key. Read the summary: chain id, `mempool.max-txs: -1`, `node id` (from `NODE_KEY_B64`),
   the consensus address, `TUNNEL_TOKEN`, and the `history:` line (`pruning=nothing
   discard_abci_responses=false tx_index=kv skip_backup=true`). `build-sdl.py` refuses
   an SDL that prunes, drops block results, state syncs or keeps the cosmovisor backup.
   `--fullnode` keeps `VALIDATOR_MNEMONIC` off the lease: the operator key signs from
   the operator's machine only.
2. **Create**, on a provider whose CPUs have **ADX**. `earthd` dies with `Illegal
   instruction` on a host without it, even for `earthd version`. Akash cannot filter
   bids by CPU feature, so name the provider. Known ADX hosts: the v1.0.0 validator's
   `akash15ksejj7g4su7ljufsg0a8eglvkje94z8qsh68a` and the backend's
   `akash1aaul837r7en7hpk9wv2svg8u78fdq0t2j2e82z`. The older `akash15tl6…` stopped
   bidding.

       bin/create.sh <tag> --fullnode --no-statesync --validator-key --node-key \
         --tunnel --first-validator-lease --provider <ADX provider> --var DSEQ

   `--first-validator-lease` is for this create only: it is how the script knows
   that a missing `akash/validator-lease.lock` means "never made one" and not "this
   clone has not pulled it". Every later `--validator-key` create without the lock
   is refused, and with the flag while a lock exists is refused too.

   `--var DSEQ` points `.env`'s `DSEQ` at the new lease, so `deploy.sh`,
   `lease-logs.py` and `lease-shell.py` target it. That is safe because nothing else is
   live.

   As soon as the deployment exists (before bids and the lease), `create.sh` writes
   `akash/validator-lease.lock` (genesis pin, DSEQ, time), and adds the provider once
   the lease is taken. **Commit it.** Every later `--validator-key` create is refused
   while the lock is uncommitted or differs from the committed copy, for the same
   genesis pin, and whenever the Console reports the locked DSEQ or `.env`'s `DSEQ`
   active, does not know it (404: another account's API key, or indexing lag), or
   cannot be read. The one override,
   `--replace-validator-lease <closed DSEQ> --genesis <pinned genesis.json>`, is for
   this lease dying **before** `genesis_time`: the Console must report that DSEQ
   `closed`, and `genesis_time` must be at least 15 minutes ahead; past it there is
   no override (section 9). An interrupted create leaves a lock naming a deployment
   that may have no lease: commit it, close that deployment, and use the override.

   Create it **with every final value**. CometBFT sleeps until `genesis_time` before
   opening any listener, so the pod is not Ready until launch. A PUT to a pod that is
   not Ready is accepted and never applied (`updated_replicas` stays 0), so anything
   forgotten here waits until after launch.
3. **Check, before `genesis_time`** (`bin/lease-logs.py --service node`):
   - `genesis sha256 <pin> — matches the release`;
   - `node id fixed from NODE_KEY_B64: <id from step 1>`;
   - `starting under cosmovisor`, with the image as the genesis binary;
   - no `Illegal instruction`, and `bin/lease-shell.py --service node -- grep -c adx
     /proc/cpuinfo` is not 0 (if the shell answers, which needs a running container);
   - no `REQUIRE_NO_CONSENSUS_KEY` refusal (the validator does not set it);
   - the 26656 host:port from the lease status. Record it for `EXTERNAL_ADDRESS` and
     the docs' P2P address;
   - `bin/lease-logs.py --service edge`: `earth-edge: rpc :26657 -> http://node:26657,
     lcd :1317 -> http://node:1317, … LCD routes, … gRPC paths over abci_query,
     backend reserve true`, and no `refusing to run as root`. `backend reserve false`
     means the SDL went out without `EDGE_BACKEND_AUTH_SHA256` (`build-sdl.py` takes it
     from `CHAIN_EDGE_TOKEN` in this repo's `.env` and refuses `--tunnel` without it). The filter is up before the node: until
     `genesis_time` it answers `502`.
4. **Public Hostnames and Cloudflare rules for `rpc.*` and `lcd.*`** (akash/README.md,
   "Public RPC and LCD"), in place before `genesis_time`. The hostnames point at the
   filter: `rpc.* -> http://edge:26657`, `lcd.* -> http://edge:1317`, never `node:*`
   (that serves the public unfiltered). Then rule 0 (Skip **rate limiting rules
   only** for the backend's `CHAIN_EDGE_TOKEN` header: the token
   `bin/gen-edge-token.sh` wrote into both `.env` files in section 2's
   prerequisite, which the lease's edge already hashes. Do not generate a new one
   here), rule 1
   (block websocket upgrades), the two per-IP rate limits in the variant your plan
   accepts, and the `genesis_chunked` cache rule. **Purge `rpc.erth.network/genesis_chunked`
   from Cloudflare's cache** (Caching → Purge Cache → Custom Purge, by URL prefix): a
   relaunch under the same hostname has a new genesis. The allowlist itself is `edge`,
   in the SDL: nothing to configure. After the node is up, run
   `bin/check-edge.py` (exit 0, every line `ok`), then that section's manual checks; a
   missing `X-Earth-Edge` means a hostname points at the node (fix it before the
   manual checks, some of which are scans). Keep `bin/check-edge.py` on a schedule from then on. The node-side limits are
   in the SDL too (`limits:`, `index:` and `edge:` lines of the dry run, `rpc_subs=0`);
   its `edge image:` line must be the digest section 1.4 pinned.
   **Network → Pseudo IPv4 must be Off** (the default; "Add header" also works, never
   "Overwrite headers"). The edge keys its per-client caps on `CF-Connecting-IP`, and
   groups IPv6 clients by /64 (`edge/filter/client.go`). With "Overwrite headers",
   Cloudflare replaces an IPv6 client's address there with a Class E IPv4 hashed from
   it, so the edge keys each address alone and one IPv6 subscriber, who holds a whole
   /64, gets a fresh set of slots per address (round-9 R9-D-2).
5. **Cloudflare no-logs settings** (NO_LOGS.md, "Cloudflare settings"): no Logpush
   job, Web Analytics and Network Error Logging off, no Zaraz or Workers, Browser
   Integrity Check and Security Level off for the API hostnames, WAF rules on Block,
   rule 0 not logging. The node and cloudflared log levels are in the SDL and checked
   by `build-sdl.py`; `edge` logs no request at all.

**Disk.** The node keeps everything (`pruning=nothing`, every block's results) on a
200Gi volume that only grows, and Akash cannot grow a volume in place: a bigger disk
is a new lease, a replay from block 1, and a validator migration. Check weekly and
after any burst of registrations, and alert at about 40% (80Gi). Half full is when a
replacement must already be syncing. Re-measure the growth in month one and turn it
into a date.

    bin/lease-shell.py --service node -- df -h /data
    bin/lease-shell.py --service node -- du -sh /data/data /data/cosmovisor

## 4. Backend and indexer (backend repo)

A **new lease**, so the state volume is new: a fresh privacy index (the old one belongs
to no running chain, and the indexer halts on a chain it does not know) and an empty
grant history.

1. **Bump the earthd pin, then release.** The pin is the two `ARG` lines in the
   backend `Dockerfile` and nothing else. Set `EARTHD_VERSION` to the launch tag and
   `EARTHD_SHA256` to its `earthd_<tag>_linux_amd64.tar.gz` line in the tag's
   `checksums.txt`:

       curl -fsSL https://github.com/zenopie/earth-network-chain/releases/download/<tag>/checksums.txt \
         | grep linux_amd64.tar.gz

   The image refuses to build on any `v0.*` (by name) or the never-run `v1.0.0` build
   (by its tarball's sha256 `16842a45…`, so a launch release re-cut under the tag
   `v1.0.0` is not blocked; a new tag name is still clearer): those cannot check the
   relaunch `MsgRegister`, so until this bump no backend release can be tagged. Commit,
   tag the backend release, and let CI build it.
2. **SDL** (`deploy/akash/deploy.yaml`): `EARTH_CHAIN_ID=earth-1`,
   `INDEXER_RPC_URL=https://rpc.erth.network:443` (CometBFT RPC on the validator),
   `INDEXER_START_HEIGHT=0`, `INDEX_DB=/app/state/privacy_index.db`. The indexer refuses
   a different chain id or block hash behind its RPC.
3. **Create** on an ADX provider (`earthd gas-check` needs ADX too). Sharing the
   validator's provider is fine: the backend reaches the chain only through the
   Cloudflare hostnames, never a provider hostname (that would hairpin inside the
   provider's cluster and hang; the backend's `build-sdl.py` refuses one):

       bin/create.py <tag> --provider <ADX provider>

   `GAS_WALLET_MNEMONIC` (the new wallet's, written 2026-10-04) and
   `CHAIN_EDGE_TOKEN` (section 2, prerequisite) are injected from the backend's `.env`; the
   backend's `build-sdl.py` refuses to build without the token.
4. **`api.erth.network`**: one connector per tunnel. Close the old lease
   `1790918719150` as soon as the new one serves, or the two connectors split
   requests.
5. **The backend skips the per-IP rate limits by its token, not its address.**
   Nothing to do per lease: rule 0 matches the `CHAIN_EDGE_TOKEN` header wherever the
   backend runs, and skips only the rate limits; every call the backend makes is in
   the filter's public allowlist anyway; the edge gives the same header a few reserved
   slots (`EDGE_BACKEND_AUTH_SHA256`, section 2, prerequisite). If the startup log says
   `CHAIN_EDGE_TOKEN is unset`, the backend works but its re-index and grant bursts
   meet `429`s; if grants fail with gas-check unavailable and a `403 … refused by the
   edge filter`, gas-check made a call the filter does not serve (a chain change to
   what gas-check reads needs `subspaceRules`/`storeKeyModules` in `edge/filter/`
   updated, and the edge image rebuilt and re-pinned (section 1.4), before that chain
   release is deployed).
6. **Purge the circuit cache once.** Backends before this release served
   `/circuits/<variant>.json.gz` as `immutable` for a year, and all 17 circuits
   changed under those names, so a Cloudflare colo may still hold an old build.
   **Caching → Configuration → Purge Cache → Custom Purge**: by prefix
   `api.erth.network/circuits/` where the plan offers it, otherwise by URL, the 17
   listed by `sed 's|.*  \(.*\)\.json$|https://api.erth.network/circuits/\1.json.gz|'
   circuits/SHA256SUMS` in the backend repo. From now on the plain names are cached for 5 minutes
   and the content-addressed names (`<variant>.<sha256>.json.gz`) never change, so
   later circuit changes need no purge.

**Check:** `/health` answers (its `grants_remaining` is 0 until section 6),
`/opt/earthd/bin/earthd version` in the container prints the launch tag, and after
genesis `/privacy/status` follows the chain height with no halt reason and
`bin/verify-trees.py --db …` exits 0.

## 5. Web app (web repo)

1. Tag the release (`vX.Y.Z`). CI builds the image and pins its digest in
   `deploy/akash/deploy.yaml`. A tag deploys nothing by itself: run the deploy.

       git pull                          # the pinned digest
       deploy/akash/deploy.sh            # in place, lease 1787052820013

2. **Cloudflare, `erth.network` zone: SSL/TLS → Edge Certificates → "Always Use HTTPS"
   on.** TLS ends at Cloudflare and nginx only sees plain HTTP from the tunnel, so the
   redirect must be Cloudflare's; one in nginx would loop. As of 2026-10-03 it was off.

**Check:**

    curl -sI http://erth.network/ | head -1                                 # 301 to https
    curl -sI https://erth.network/ | grep -i referrer-policy                # no-referrer
    curl -i https://erth.network/.well-known/apple-app-site-association     # 200 application/json
    curl -i https://erth.network/.well-known/assetlinks.json                # 200 application/json

## 6. Launch

At `genesis_time`:

1. Blocks are produced: `rpc.erth.network/status` height rises, and
   `earliest_block_height` is 1.
2. Full history: `/block_results?height=1` answers.
3. The indexer follows (`/privacy/status`).
4. **Fund the gas wallet** from the validator's operator account, with transparent
   ERTH, from the operator's machine. Import the operator key there once, typing
   `VALIDATOR_MNEMONIC` at the prompt (never into a file or a shell argument), and
   check the address:

       earthd keys add operator --recover --keyring-backend os
       earthd keys show operator -a --keyring-backend os   # earth1n6amvkgfrrgy6ulhurewnm0endkgye69fkcapr

    The validator holds 900 ERTH liquid at genesis
   (1,000 less the 100 self-bond); a private tx costs about 0.01 ERTH at 0.005uerth.

       earthd tx bank send earth1n6amvkgfrrgy6ulhurewnm0endkgye69fkcapr \
         earth13ysugyz4la7kfrmgsfdhk203ujt0jgcpw2avmg <amount>uerth \
         --from operator --keyring-backend os --chain-id earth-1 --node https://rpc.erth.network:443 \
         --gas auto --gas-adjustment 1.5 --gas-prices 0.005uerth

   Then `earthd query tx <hash>`: code 0 there, not just at broadcast. The backend's
   `/health` `grants_remaining` rises. Until this is done, every `/gas/register` fails
   and new users cannot make their first transaction.
5. **Smoke tests**, each from a **store build** of the apps. Read every result with
   `earthd query tx <hash>`. Code 0 from a broadcast only means the tx entered the
   mempool. A private tx has no `message.sender`, so search by hash; a failed one
   is invisible to a `message.action` search.
   - registration, with backend gas, and its own first ANML note (daily claims open the
     day after tomorrow);
   - claim a handle; pay it from the other phone, and from the web app (Shield to
     `@handle` via Keplr); open `https://erth.network/ref/<handle>` on a phone with
     the app (App Link) and without it (the web fallback);
   - a shielded send, and an unshield;
   - a swap and an ANML sell from notes; Buy ANML from the web app;
   - a delegation (it settles at the next epoch end, a day later); a second delegation
     to the same validator merges into the one stake note;
   - a redelegation to another validator;
   - an undelegation (its payout arrives by itself after unbonding; check it is
     queued with `Query/UnbondPayout`);
   - an assembly vote and a stake vote on a test proposal. Both houses must pass it.
6. **Publish the docs** with the placeholders filled (tag, genesis sha256, node id,
   P2P address).

## 7. After launch

- `EXTERNAL_ADDRESS` in `akash/deploy.yaml` = the validator's 26656 host:port, applied
  in place (the pod is Ready now) with the lease's own flags:
  `bin/deploy.sh <tag> --fullnode --no-statesync --validator-key --node-key --tunnel`.
- `bin/lease-logs.py` and the disk check (section 3) on a schedule.
- Run the web app's `npm run check:dex-live` against the live LCD.
- The IBC relayer: `akash/deploy.yaml` has the old path's ids commented out. Link anew
  with `LINK_ON_START=true` for one deploy, pin the new ids, put it back, and fund the
  relayer key on both chains (transparent ERTH here). Deploy with the validator's
  flags plus `--relayer`, which injects `RELAYER_MNEMONIC` (akash/README.md).
- Bump cosmovisor when a release contains cosmos-sdk #23720. Until then its upgrade
  detection can lose a race on a small node.

## 8. Fixes before governance works

Every governance proposal, software upgrades included, needs 2/3 of the human votes
cast in the assembly as well as the stake vote (x/assembly). At launch no human is
registered, so **no proposal can pass until registered humans vote YES**. Until then
a consensus fix cannot go through `x/upgrade` and cosmovisor. It is a coordinated
binary swap at a chosen height H, done by the operator (the one validator):

1. **Gate the fix on height.** The new binary must behave exactly like the running one
   below H and apply the change from H on (`ctx.BlockHeight() >= H`); a store
   migration runs once, in the block at H. Test that it agrees with the running chain
   below H: a scratch keyless full node (`--fullnode --no-statesync`, peered to the
   validator) on the new binary must sync to the tip with no app-hash error.
2. **Pick H** a few hours to a day ahead, tag the release, and announce H and the tag
   in the docs for anyone running a node (a node still on the old binary at H stops
   agreeing with the chain).
3. **Rebuild the node image on the new tag** (gate 1.5). `deploy.sh <newtag>` is
   refused until `node/base.pin` and both image lines' `# FROM` name the new tag's
   chain image, and the image's registry labels say the same. First record the
   rollback commit, the deploy commit the validator runs now, with nothing
   uncommitted:

       git status --porcelain              # empty
       ROLLBACK=$(git rev-parse HEAD)      # write it down with <oldtag>

   Then, with no other edit in between (so the rollback commit differs from the swap
   only in the image pins):

       bin/build-node.sh --base <newtag>   # writes node/base.pin; commit it
       bin/build-node.sh --pin             # builds on it, pushes, pins node + relayer; commit akash/deploy.yaml
       bin/deploy.sh <newtag> <validator's flags> --print >/dev/null && echo builds

   The genesis is unchanged, so the new chain image must carry the same baked genesis
   (`akash/genesis.sha256`); the node build refuses one that does not. Point
   `edge/conformance/chain.pin` at the new tag too, rerun conformance and
   `bin/build-edge.sh --pin` (gate 1.4); fix `edge/` first if conformance fails.
4. **Swap while the pod is Ready**, before H: `bin/deploy.sh <newtag>` with the
   validator's flags. The pod restarts on the new image. Cosmovisor's `current`
   symlink points at `genesis/` (it creates that link on its first run, and only an
   `x/upgrade` plan repoints it to `upgrades/<name>`), and the entrypoint re-links
   `genesis/bin/earthd` to the image's binary on every start, so the new image's
   binary is what runs. Check `earthd version` and that blocks continue.
5. **After H**, check the fix took effect. If it changes what `earthd gas-check`
   accepts, bump the backend's earthd pin the same way (section 4.1).

**Rollback.** A swap can fail two ways: the new image dies on start (a bad build, an
ADX or path problem, a migration that panics at boot), or it runs fine below H and
panics at H, the first block its new code runs. Either way the validator crash-loops,
the chain stops, and what Akash allows is narrow:

- A PUT (`deploy.sh`) to a pod that is not Ready is accepted and **never applied**. So
  `deploy.sh <oldtag>` alone does not roll back a crash-looping validator.
- What does: the provider deletes pod `node-0`, after which Kubernetes starts it from
  the current manifest. Closing the lease is never a rollback: it destroys the volume,
  and with it the chain (section 9).

So, before every swap:

1. **Provider contact, confirmed.** Name the provider's operator, the channel, and
   their response time, and get a yes to "delete pod `node-0` of lease `<DSEQ>` on
   request" for the swap window. No confirmed contact, no swap. (The same arrangement
   is what a `halt-height` stop needs; see below.)
2. **The rollback ready, as it will be run.** From the swap checkout,
   `deploy.sh <oldtag>` is refused (`node/base.pin` names the new tag). The rollback
   deploys from a worktree at the rollback commit (swap step 3), which pins the old
   node image and has every other SDL value the validator runs (`EXTERNAL_ADDRESS`,
   relayer ids). `.env` is not in git, so link it in:

       git worktree add ../deploy-rollback "$ROLLBACK"
       ln -s "$PWD/.env" ../deploy-rollback/.env
       (cd ../deploy-rollback && bin/deploy.sh <oldtag> <validator's flags> --print >/dev/null && echo builds)

   Run this after the swap commits exist, not before, and keep the worktree until H
   has passed. Write down the exact command without `--print`. Pinned-digest
   alternative, if the worktree is lost: `git checkout "$ROLLBACK" -- node/base.pin
   akash/deploy.yaml` in the main checkout restores the old pins (the old node image is
   still in the registry under its digest; `bin/check-node-image.py` verifies it), but
   it also reverts any SDL edit made after the rollback commit, so prefer the worktree.
3. **Rehearse H, not just the sync.** The scratch node from step 1 proves start-up and
   agreement below H, on the new image, on an ADX host. To run H itself before the
   validator does: stop the scratch node at H-1 (`EARTHD_HALT_HEIGHT=H-1` is fine on a
   keyless scratch node), copy its data, and run `earthd in-place-testnet` on the copy
   with a **throwaway** validator key (never `PGqvPN4C…`) and a different chain id,
   then let it produce H and a few blocks. That exercises the store migration and the
   H code on the real state. Keep the scratch node itself following the validator
   through H and compare its app hash at H+1 with the validator's.
4. **Swap early.** Swap hours before H, so a start-up failure is found while the old
   binary is still correct for every height.

Then the rollback, by failure:

- **Dies on start, before H**: from the rollback worktree, `bin/deploy.sh <oldtag>`
  with the validator's flags (accepted, not yet applied), then ask the provider to
  delete `node-0`. Afterwards revert the swap commits in the main checkout (or
  rebuild on a fixed tag), so the next deploy from it is not the failed image. The old binary resumes where the chain stopped:
  every block the new binary committed below H is one the old binary would have
  produced. Downtime is the provider's response time.
- **Panics while executing H** (in `FinalizeBlock`, before the app commits H): the
  same. CometBFT may already hold block H as decided (it stores the block before
  executing it), but the app's state is still H-1. On restart the old binary's
  handshake replays block H's transactions under the old rules and the chain goes on;
  block H's header carries H-1's app hash, which both binaries agree on. Postpone the
  fix to a new H with a fixed build. If the panic came before the block was decided,
  the old binary simply proposes H again; the FilePV state on the volume refuses a
  conflicting vote in the same round and the next round proceeds, which is not double
  signing.
- **The app committed H, then a crash at H+1 or later**: the old binary cannot follow,
  because the committed state already has the change. There is no rollback, only fix
  forward: a corrected new binary (its node image rebuilt as in step 3), the same
  `deploy.sh` plus provider pod deletion.
  This is why step 3's rehearsal of H matters. (A scratch node that also ran H on the
  new binary must be resynced if the chain then went the other way.)

Why not a `halt-height` stop: with `EARTHD_HALT_HEIGHT=H` the node refuses block H
and exits, the container crash-loops, and a PUT to a pod that is not Ready is accepted
and never applied. The new image would then wait for the provider to delete pod
`node-0`. Use a halt only if the fix cannot be height-gated, and only after the
provider has agreed to delete the pod on request. Once humans are registered, use
governance; then cosmovisor's `current` outranks the image.

## 9. Rollback

- **Before `genesis_time`**: nothing is irreversible. Close the new leases, fix, pick a
  **new** `genesis_time`, re-pin, re-tag, recreate (the new pin passes `create.sh`'s
  lock). To recreate for the *same* genesis while `genesis_time` is still at least 15
  minutes ahead, close the dead lease and pass `--replace-validator-lease <its DSEQ>
  --genesis <pinned genesis.json>`; first check the Cloudflare dashboard shows no
  connector on the chain tunnel (`create.sh` itself requires the Console to report the
  old DSEQ `closed`; a 404 is refused). That is the whole safety check: before `genesis_time` the key has signed
  nothing.
- **Fails at height 1** (InitChain panic, app hash mismatch): the pod crash-loops, and a
  crash-looping pod cannot be fixed by a PUT. Either the provider deletes pod `node-0`,
  or close the lease and create a new one. Nothing is lost at height 1. Fix the genesis,
  set a new `genesis_time`, re-pin the sha, re-tag, and recreate.
- **A wrong parameter after launch**: governance once humans can vote (both houses
  must pass it), section 8 until then. While only the operator holds anything, a
  fresh genesis with a new `genesis_time` can be cheaper; ask first.
- **The validator's volume is lost** (the provider disappears or loses the disk, or
  the lease closes on escrow or by mistake): **the chain is lost.** That volume is
  the only copy of earth-1's state and blocks. There is no second node, no snapshot
  off the lease (the state-sync snapshots live on the same volume), and the indexer
  holds note and nullifier rows, not app state. A new lease is **not** a recovery:
  with `.env`'s keys it would join the image genesis at height 0 and produce a
  different chain under the same chain id, with every balance and registration gone.
  - **Never sign heights 1.. again with `PGqvPN4C…` under `earth-1`.** The original
    blocks at those heights carry that key's signatures; a second set is double-sign
    evidence against the only validator, and it tells every wallet and the indexer
    that history changed (the indexer halts on the block-hash mismatch). Starting
    over means a new genesis, a new `genesis_time` and a **new consensus key** (add
    `PGqvPN4C…` to `used_consensus_keys` in `launch/launch.json`, which
    `bin/build-sdl.py`, `bin/check-genesis.sh` and the chain's `scripts/ceremony.sh`
    all read, and set `consensus_pubkey` to the new key), decided by a human, not a
    script.
  - So protect the volume: keep the escrow funded, never close this lease, and check
    the disk (section 3).
- **Two nodes and the consensus key.** Never run two nodes holding the consensus key.
  `--fullnode` does not remove a key already on a volume: a former validator's volume
  signs from disk (that caused the 69-block fork on 2026-09-01); the entrypoint
  refuses a keyless node whose volume holds a key it did not mint as a throwaway.
