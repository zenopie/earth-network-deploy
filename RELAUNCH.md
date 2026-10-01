# Privacy relaunch runbook (fresh genesis)

The privacy chain (shielded ERTH/ANML, private personhood, private staking;
plan: `~/.claude/plans/earth-private-personhood-anml.md`) launches from a new
genesis. No state migrates and there are no upgrade handlers. Everything on the
current `earth-1` is abandoned at the cutover.

This is the order of operations, the checks at each step, and what to do when a
step fails. Items marked **TODO** need a decision from the operator before the
step they gate.

## 0. Decisions that gate everything

| # | Decision | Where it lands | Status |
| --- | --- | --- | --- |
| D1 | **New consensus key**, chain id stays `earth-1` (decided 2026-10-01) | new `priv_validator_key.json` → regenerate the gentx in chain `networks/genesis/gentx/`; `.env` `PRIV_VALIDATOR_KEY_B64` | **TODO**: generate the key and gentx in the genesis ceremony |
| D2 | **Genesis final**, its sha256 | `akash/genesis.sha256` (pin by hand), `bin/check-genesis.sh` | **TODO**: Phase 5 is finalising `networks/genesis.json` |
| D3 | **genesis_time** | `networks/genesis/chain.json` | **TODO**: a real UTC instant after the apps are live in both stores |
| D4 | **Launch tag** (`earthd` release) | image digest via `bin/digest.sh`, backend `EARTHD_VERSION`/`EARTHD_SHA256`, docs `join.md` | **TODO** |
| D5 | **Lease sizing** | `akash/deploy.yaml` (validator 2 CPU / 4Gi / 100Gi), `akash/deploy-archive.yaml` (2 CPU / 4Gi / 200Gi), backend 4Gi state | **TODO**: confirm archive 200Gi |
| D6 | Devnet accounts out of genesis (faucet `earth1s7rg…`, hot wallet) | chain `networks/genesis/accounts.json` | **TODO** (Phase 5) |

### Consensus key: generate a new one (decided 2026-10-01)

The chain id stays `earth-1`; the old chain was deleted (its last lease, `1790678074849`, was closed on 2026-10-01). The relaunch still needs a **new consensus key**:
- The old key `90603989D53D23401BB03C56C6728AE95A5AF8E1` signed every earlier `earth-1` block, and those blocks were public.
- An old signature at height *h*, paired with the new chain's signature at *h* from the same key under the same chain id, is valid double-sign evidence. That would tombstone the only validator.
- With a new key, old signatures belong to a key the new chain has never seen.

In the ceremony:
1. `earthd init` on a clean home to get a fresh `priv_validator_key.json`.
2. Regenerate the gentx with it.
3. Put the key in `.env` as `PRIV_VALIDATOR_KEY_B64`.
4. Destroy the old key. Don't keep it: it has no use and is only a liability.

## 1. Release gating

Nothing goes on a lease until all of these exist. **The apps come first.**
Registration, claims, votes and every private transfer need the new circuits
and the new tx formats. An old build cannot do any of them on the new chain,
and store review takes days.

1. **Chain release** at the launch tag: binaries (`linux/amd64`, `linux/arm64`),
   `checksums.txt`, `genesis.json`, and the image on ghcr. Check:
   - `bin/check-genesis.sh <tag> --chain <chain checkout at the tag>` passes.
   - `bin/digest.sh <tag>` resolves.
   - In a container from that digest, `earthd version --long` shows the tag's
     commit. The SDLs' committed digests are stale placeholders; what runs is
     what `deploy.sh`/`create.sh` pinned.
2. **Mobile apps** (Android, then iOS) with the membership and transfer
   circuits, the new key derivation, the indexer sync and the chain id, live in
   **both stores before `genesis_time`**.
   - iOS bundles the circuits at build time from the Android assets folder. Build
     iOS after the final circuit commit, and hash the circuits inside the built
     `.app` against the repo copies. A stale bundle fails registration at the
     proof-binding check, and the error looks like an iOS bug.
   - The verifying keys in genesis must be the ones the shipped apps prove
     against.
3. **Backend release** with `EARTHD_VERSION`/`EARTHD_SHA256` at the launch tag.
   `/gas/register` runs `earthd gas-check registration` against the new chain's
   rules and shields the dust to `pc_gas`. The old binary cannot check the new
   registration format.
4. **Web app**: read-only shielded views, transparent Keplr actions, the new
   chain id.
5. **Docs**: the `privacy/main` branch of earth-network-docs. Fill in the
   `TODO(relaunch)` placeholders in `docs/run-a-node/join.md` (launch tag,
   genesis sha, node id, chain id) when D1 to D4 are final, and publish at launch.

## 2. Genesis ceremony

1. Freeze the chain branch. `make genesis` and `make genesis-check` in the
   chain repo, then `go test ./networks/...`.
2. Confirm in `networks/genesis.json`:
   - `chain_id` = D1, `genesis_time` = D3, one gentx with the intended key (D1);
   - staking `max_entries` 32. Daily epochs need it; earth-1 had 7;
   - the membership and transfer verifying keys present, matching the shipped apps;
   - no dev `uanml` balances, no devnet accounts (D6);
   - pool 1 reserves as intended.
3. Write its sha256 into `akash/genesis.sha256` and commit. Run
   `bin/check-genesis.sh --chain <chain repo>`.
4. Tag the release (section 1.1) from that exact commit. The image's baked
   `/etc/earth/genesis.json` is what every node installs.

## 3. Validator

**Use a new lease.** An in-place reset with `RESET_ON_GENESIS_MISMATCH=1` would
keep the p2p port, but it deletes only `config/` and `data/`. A
`cosmovisor/current` or `upgrades/` left on the volume by the old chain
outranks the image's genesis binary, so the node would start the old binary on
the new genesis and crash-loop. A crash-looping pod cannot be replaced by a PUT.
A new volume carries none of that, and 100Gi is the size already chosen after
the disk-full halt.

1. **Park the old lease first** (`akash/deploy-parked.yaml`, deployed to
   `DSEQ`). Parking stops earthd and the old `cloudflared`, which shares
   `TUNNEL_TOKEN`. Leaving the old connector up puts a second replica on
   rpc/lcd.erth.network, and roughly half the requests go to the parked node.
   Then move the old volume's key aside, so a later unpark cannot sign:

       bin/lease-shell.py --service node --dseq <old> -- \
         mv /data/config/priv_validator_key.json /data/config/priv_validator_key.json.REMOVED-see-env

   `--fullnode` keeps keys out of the SDL. It does not remove a key already on
   the volume. That produced a 69-block fork on 2026-09-01.
2. **Dry run**, sending nothing:

       bin/create.sh <tag> --fullnode --no-statesync --validator-key --node-key \
         --tunnel --sdl-only

   Read the summary: chain id, `mempool.max-txs: -1`, `node id` (from
   `NODE_KEY_B64`; this is the id the archive node and joiners dial), consensus
   address, `TUNNEL_TOKEN`.
3. **Create**, on an ADX provider. `earthd` dies with `Illegal instruction` on
   a host without ADX. The current chain's EPYC provider
   `akash15tl6v6gd0nte0syyxnv57zmmspgju4c3xfmdhk` has it.

       bin/create.sh <tag> --fullnode --no-statesync --validator-key --node-key \
         --tunnel --provider akash15tl6v6gd0nte0syyxnv57zmmspgju4c3xfmdhk --var NEW_DSEQ

   Create it **with every final value**. CometBFT sleeps until `genesis_time`
   before opening any listener, so the pod is not Ready until launch, and a PUT
   to a pod that is not Ready is accepted and never applied
   (`updated_replicas` stays 0). Anything forgotten here waits until after
   launch.
4. **Checks before `genesis_time`**, via `bin/lease-logs.py --dseq $NEW_DSEQ`:
   - `genesis sha256 <pin> — matches the release`;
   - `node id fixed from NODE_KEY_B64: <id from step 2>`;
   - `starting under cosmovisor`, with the image as the genesis binary;
   - no `REQUIRE_NO_CONSENSUS_KEY` refusal (the validator does not set it);
   - the 26656 host:port from the lease status. Record it for section 4.
5. After launch, set `EXTERNAL_ADDRESS` to that host:port with an in-place PUT.
   Repoint `DSEQ` in `.env` at the new lease.
6. **Close the old lease** only once the new chain has run cleanly for a few
   days. Closing destroys the old chain's state, which is the only rollback
   (section 8).

Disk: keep the volume **under half full before any upgrade height**, or set
`UNSAFE_SKIP_BACKUP=true` for that upgrade. Cosmovisor's pre-upgrade backup of
`data/` is what filled 20Gi and ended the v0.9.2 chain. Volumes cannot grow in
place.

## 4. Archive / full-history RPC (for the indexer)

`akash/deploy-archive.yaml`: keyless, never state-synced, `pruning=nothing`,
`min-retain-blocks=0`, `discard_abci_responses=false`, `tx_index=kv`, the
no-op mempool, cosmovisor with the backup skipped, 200Gi (D5). `build-sdl.py`
refuses it with any key, with state sync, with pruning, or on the validator's
tunnel.

1. Fill `PERSISTENT_PEERS` with `<node id from 3.2>@<host:port from 3.4>`.
   `build-sdl.py` refuses an unfilled `<placeholder>`.
2. Create it around `genesis_time`, on an ADX provider:

       bin/create.sh <tag> --sdl akash/deploy-archive.yaml --fullnode --no-statesync \
         --provider <ADX provider> --var ARCHIVE_DSEQ

   It runs `REQUIRE_NO_CONSENSUS_KEY=1`, so it refuses to start if a consensus
   key ever appears on the volume.
3. Once `catching_up: false`, attach its own tunnel:

       bin/deploy.sh <tag> --sdl akash/deploy-archive.yaml --dseq $ARCHIVE_DSEQ \
         --fullnode --no-statesync --tunnel --tunnel-var ARCHIVE_TUNNEL_TOKEN

   On Cloudflare, map `archive-rpc.erth.network` to `http://node:26657`.
4. Checks:
   - `/status` shows `earliest_block_height: 1`;
   - `/block_results?height=1` answers;
   - `/tx?hash=…` answers for a known tx.

## 5. Backend and indexer

The backend SDL (earth-network-backend, `deploy/akash/deploy.yaml`) now asks for
a **4Gi** state volume for the privacy index. Akash cannot grow a volume in
place, so this is a **new lease**: `bin/create.py --provider <ADX provider>` in
that repo. `earthd gas-check` needs ADX too. The old lease's state (grant replay
ids for the old chain) has no value on the new chain.

1. Release the backend with `EARTHD_VERSION`/`EARTHD_SHA256` at the launch tag
   (section 1.3).
2. SDL env:
   - `EARTH_CHAIN_ID` = D1.
   - `INDEXER_RPC_URL=https://archive-rpc.erth.network:443`. **TODO** in the
     backend repo: it currently reads `rpc.erth.network`, the validator. That
     works, because default pruning keeps every block and its results, but it
     puts the indexer's full-range reads on the block producer.
   - `INDEXER_START_HEIGHT=0` and a fresh `INDEX_DB`. The indexer refuses a
     different chain id or block hash behind its RPC.
3. Fund the gas hot wallet with **transparent** ERTH after launch. It shields
   dust to each `pc_gas`. Fund it from the validator's account with a key that
   has never been on a laptop, not from a devnet genesis balance.
4. Cut `api.erth.network` over: park the old backend's `cloudflared` before the
   new one connects (the same one-token rule as section 3.1), or give the new
   lease a new token.
5. Checks:
   - `/privacy/status` follows the chain height with no halt reason;
   - `bin/verify-trees.py --db …` exits 0;
   - `/gas/register` pays a real registration from a store build.

## 6. Launch day

At `genesis_time`:

1. Blocks are produced: `rpc.erth.network/status` height rises.
2. The archive node and the indexer follow.
3. Smoke tests, each from a store build of the app. **Read the execution result
   with `query tx <hash>`.** Code 0 from a broadcast only means the tx entered
   the mempool. A private tx has no `message.sender`, so search by hash:
   - registration, with backend gas;
   - an ANML claim. Daily claims open the day after tomorrow for a new
     registration, so test with the registration's own ANML note;
   - a shielded transfer, and an unshield;
   - a delegation, whose rate and supply update at the next epoch end, one day
     later;
   - an ANML sell from a note;
   - an assembly vote and a stake vote on a test proposal. Both houses must pass.
4. Publish the docs branch, after filling its placeholders.

## 7. After launch

- `EXTERNAL_ADDRESS` on the validator (section 3.5) and on the archive node.
- The IBC relayer: `akash/deploy.yaml` has the old path's ids commented out.
  Link anew with `LINK_ON_START=true` for one deploy, pin the new ids, and fund
  the relayer key with transparent ERTH.
- Close the old validator lease (section 3.6) and the old backend lease.
- Bump cosmovisor when a release contains cosmos-sdk #23720. Until then its
  upgrade detection can lose a race on a small node.

## 8. Rollback

- **Before `genesis_time`**, nothing is irreversible except closing the old
  lease. To abort: close the new lease, put the key back on the old volume
  (`mv …REMOVED-see-env` back), and deploy the normal SDL to the old `DSEQ`.
  Never run both. If the chain id and key are unchanged (D1), the old and new
  nodes would be signing equivocation evidence against each other.
- **Fails at height 1** (InitChain panic, app hash mismatch): the pod
  crash-loops, and a crash-looping pod cannot be fixed by a PUT. Either the
  provider deletes pod `node-0`, or you close the lease and create a new one.
  Nothing is lost at height 1. Fix the genesis, set a **new** `genesis_time`,
  re-pin the sha, re-tag, and recreate.
- **A wrong parameter found after launch**: consensus changes go through
  governance, never an in-place binary swap. Both houses must pass it, so the
  assembly needs registered humans who can vote, which is another reason the
  apps ship first. Early on, while only the operator holds anything, a fresh
  genesis (with a new `genesis_time`) can be cheaper. That is what replaced the
  2026-08-28 morning attempt.
- **Halt at an upgrade height**: see the 2026-08-30 v0.6.0 recovery (stage the
  binary into `cosmovisor/upgrades/<name>/bin` with its `upgrade-info.json`,
  using a temporary `command:` override). That only works while a PUT can still
  replace the pod.
- **Validator lease lost**: `.env` holds `PRIV_VALIDATOR_KEY_B64` and
  `NODE_KEY_B64`, so a new lease can sign again. Before promoting any replacement,
  do the four checks in the 2026-09-01 migration: `priv_validator_state.json`
  height and signbytes hash, the new node at height 0, and matching genesis
  sha256.
