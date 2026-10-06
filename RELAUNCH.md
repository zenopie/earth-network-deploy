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
| `akash/genesis.sha256` | Pins `acb96128…`, the chain repo's `networks/genesis.json` at `6d3500a`. That is the **pre-ceremony** genesis: past `genesis_time`, placeholder operator, devnet accounts. `bin/check-genesis.sh` FAILS it (past `genesis_time`, placeholder operator, devnet accounts) unless given `--allow-placeholder`. Replaced at the ceremony (section 2, step 4). |

**One node.** The validator is also the full-history node: it serves
`rpc.erth.network` and `lcd.erth.network` through its tunnel, and the privacy indexer
reads `block_results` from it from height 1. There is no separate archive lease.

## 1. Gates: nothing goes on a lease until these exist

1. **The final audit is closed** on every repo's final code (no Medium or higher open).
2. **Store builds of both apps, live before `genesis_time`.** Registration, claims,
   votes and every private tx need the final circuits, tx formats and chain id. An old
   build can do none of them on the new chain, and review takes days.
   - **iOS (TestFlight):** archive from the mobile release branch after the final
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
3. **Chain release candidate**: everything merged, `make genesis-check` and the test
   suite green. The tag is cut at the ceremony (section 2), because the genesis is
   baked into the image.
4. **Backend release** with `EARTHD_VERSION` / `EARTHD_SHA256` (backend `Dockerfile`)
   bumped to the launch tag. `/gas/register` runs `earthd gas-check registration`,
   the chain's own check: an older binary cannot check the new formats. So this
   release follows the chain tag (section 4.1).
5. **Web app release** from its release branch: `npm run build` and `npm run check`
   pass (`check:dex-live` needs the chain; run it after launch).
6. **Docs**: the docs release branch, with the `TODO(relaunch)` placeholders in
   `docs/run-a-node/join.md` (launch tag, genesis sha256, node id, P2P address) filled
   in once sections 2 and 3 have the values. Published at launch.

## 2. Genesis ceremony (chain repo)

Pick **`genesis_time`** at the ceremony, not before: a real UTC instant after the store
builds are live and when you have hours free to watch the launch. Emission and the POL
burn are prorated from `genesis_time`, so a time already past pays the whole gap out at
height 2.

1. **Read the key facts off the `.env`**, sending nothing (no tag needed; the image
   line stays the committed placeholder):

       python3 bin/build-sdl.py . /dev/null --fullnode --no-statesync \
         --validator-key --node-key --tunnel

   It prints the `node id` (from `NODE_KEY_B64`) and the consensus address and pubkey
   (from `PRIV_VALIDATOR_KEY_B64`). It refuses a key whose pubkey is not `PGqvPN4C…`
   (`EXPECTED_CONSENSUS_PUBKEY`), the old chain's `kTMzo…`, or an address that does not
   derive from the pubkey.
2. **Run the ceremony** in the chain repo, on the operator's machine:

       scripts/ceremony.sh --genesis-time <RFC3339, UTC> \
         --pubkey '{"@type":"/cosmos.crypto.ed25519.PubKey","key":"PGqvPN4CxEkxvvh3tSBX0SGeBgjMdqQwZkdHt8FRLm4="}' \
         --memo-peer <node id>@<host>:26656 --moniker <name>

   It reads only the `VALIDATOR_MNEMONIC` line of this repo's `.env` (`--env-file`,
   else `$EARTH_DEPLOY_ENV`, else `../earth-network-deploy/.env` beside the chain
   checkout), never prints it, and refuses a mnemonic that is not
   `earth1n6amvkgfrrgy6ulhurewnm0endkgye69fkcapr`. All or nothing, it:
   - swaps the placeholder validator `earth14e6sqtf5y7mtzwykqreewe9kg3w94t0f25d54a`
     for the operator (same 1,000 ERTH) in `networks/genesis/accounts.json`, and
     removes the devnet faucet `earth1s7rgs…` and the old gas wallet `earth1jtc2z…`;
   - writes `genesis_time` (refused unless in the future) to `chain.json`;
   - signs a new gentx with `--pubkey` (no private consensus key is written), keeping
     the placeholder's self-delegation (100 ERTH) and commission. A key that signed an
     earlier earth-1 (`kTMzo…`, consensus address `90603989…`) is refused;
   - rebuilds the genesis (`scripts/build-genesis.sh`, then `--check`) and runs the
     genesis tests with `EARTH_REQUIRE_CEREMONY=1`, then prints the sha256.

   Pass `--memo-peer` and `--moniker`: the defaults are the placeholder's
   `4284b363…@192.168.0.2:26656` (a LAN address) and `earth-akash-devnet`. The memo is
   informational; the lease's host:port is not known yet, so a planned hostname is
   fine.
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
   and commit.

**Check:**

    bin/check-genesis.sh <tag> --chain <chain checkout at the tag>   # no FAIL, no --allow-placeholder
    bin/digest.sh <tag>                                              # resolves to ghcr...@sha256

and, in a container from that digest, `earthd version --long` shows the tag's commit.

## 3. Validator lease

A **new lease** with a new 200Gi volume. Nothing is parked: no old node is running.
Before creating it, the Cloudflare dashboard must show **no** active connector on the
chain tunnel (one token, one live connector).

1. **Dry run**, sending nothing:

       bin/create.sh <tag> --fullnode --no-statesync --validator-key --node-key \
         --tunnel --sdl-only

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
         --tunnel --provider <ADX provider> --var DSEQ

   `--var DSEQ` points `.env`'s `DSEQ` at the new lease, so `deploy.sh`,
   `lease-logs.py` and `lease-shell.py` target it. That is safe because nothing else is
   live.

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
     the docs' P2P address.
4. **Cloudflare rules for `rpc.*` and `lcd.*`** (akash/README.md, "Public RPC and LCD
   limits"): the block rule and both rate limits, in place before `genesis_time`. The
   node-side limits are already in the SDL (`limits:` line of the dry run).

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

   The image refuses to build on `v1.0.0` (or any `v0.*`): those cannot check the
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

   `GAS_WALLET_MNEMONIC` (the new wallet's, written 2026-10-04) is injected from the
   backend's `.env`.
4. **`api.erth.network`**: one connector per tunnel. Close the old lease
   `1790918719150` as soon as the new one serves, or the two connectors split
   requests.

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
3. **Swap while the pod is Ready**, before H: `bin/deploy.sh <newtag>` with the
   validator's flags. The pod restarts on the new image; no upgrade has happened yet,
   so cosmovisor has no `current` and runs the image binary. Check `earthd version`
   and that blocks continue.
4. **After H**, check the fix took effect. If it changes what `earthd gas-check`
   accepts, bump the backend's earthd pin the same way (section 4.1).

Why not a `halt-height` stop: with `EARTHD_HALT_HEIGHT=H` the node refuses block H
and exits, the container crash-loops, and a PUT to a pod that is not Ready is accepted
and never applied. The new image would then wait for the provider to delete pod
`node-0`. Use a halt only if the fix cannot be height-gated, and only after the
provider has agreed to delete the pod on request. Once humans are registered, use
governance; then cosmovisor's `current` outranks the image.

## 9. Rollback

- **Before `genesis_time`**: nothing is irreversible. Close the new leases, fix, pick a
  **new** `genesis_time`, re-pin, re-tag, recreate.
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
    `PGqvPN4C…` to the used-key lists in `bin/build-sdl.py` and the chain's
    `scripts/ceremony.sh`), decided by a human, not a script.
  - So protect the volume: keep the escrow funded, never close this lease, and check
    the disk (section 3).
- **Two nodes and the consensus key.** Never run two nodes holding the consensus key.
  `--fullnode` does not remove a key already on a volume: a former validator's volume
  signs from disk (that caused the 69-block fork on 2026-09-01); the entrypoint
  refuses a keyless node whose volume holds a key it did not mint as a throwaway.
