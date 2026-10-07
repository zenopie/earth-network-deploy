package filter

// What lcd.erth.network serves: the GET routes the wallets, the web app, the
// backend (cosmpy) and the docs use, POST broadcast and simulate, and CORS
// preflights for them. Inventory: akash/README.md, "What
// the clients call". Each route names its gRPC method, and `page` says
// whether its request takes a PageRequest; conformance/ checks both against
// the protos. The LCD runs these queries outside the node's ABCI mutex; the
// RPC's abci_query serves only its own short list (rpcpolicy.go).
//
// Path parameter types (lcd.go segTypes): addr bech32, hash 64 hex, uint
// digits, hex, name [A-Za-z0-9_-.]. A route's parameters are the only query
// parameters forwarded; pagination is added where the method takes it.

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

type lcdSpec struct {
	method  string
	pattern string
	grpc    string // full gRPC method; "" for the tx service (never over abci_query)
	page    bool   // the request has a PageRequest: pagination.* served, limit injected
	params  map[string]string
	class   string // light, bulk, txhash, query, search, broadcast, simulate (default query)
	body    []string
	maxResp int64  // answer byte ceiling for public requests (0: none; forward.go)
	shed    string // shedding key prefix (shed.go); the pattern must end in {hash}
}

var (
	reDenom  = `^[a-zA-Z][a-zA-Z0-9/:._\-]{1,127}$`
	reUint   = `^[0-9]{1,20}$`
	reLimit  = `^([1-9][0-9]{0,2}|1000)$`
	reText   = `^[^\x00-\x1f\x7f]{0,128}$`
	reBondSt = `^BOND_STATUS_(BONDED|UNBONDING|UNBONDED|UNSPECIFIED)$`
)

var lcdSpecs = []lcdSpec{
	// --- tx -------------------------------------------------------------
	// CheckTx: takes the ABCI mutex, so it is in the broadcast class with
	// the RPC's broadcasts (limit.go).
	{method: "POST", pattern: "/cosmos/tx/v1beta1/txs", class: "broadcast",
		body: []string{"tx_bytes", "txBytes", "mode"}},
	{method: "POST", pattern: "/cosmos/tx/v1beta1/simulate", class: "simulate",
		// cosmpy simulates with the legacy JSON `tx`; the wallets send tx_bytes.
		body: []string{"tx_bytes", "txBytes", "tx"}},
	// Commit polls and the wallets' activity: a point lookup in the tx
	// index, its answer sized by what the tx stored (txhash, limit.go; the
	// ceiling, forward.go), and shed by hash once over it (shed.go).
	{method: "GET", pattern: "/cosmos/tx/v1beta1/txs/{hash}", class: "txhash", maxResp: maxRespTxLCD, shed: "lcd-tx"},
	// The explorer's search: one block's txs (lcd.go checkSearch).
	{method: "GET", pattern: "/cosmos/tx/v1beta1/txs", class: "search", maxResp: maxRespSearch, params: map[string]string{
		"query":    searchQuery,
		"order_by": `^ORDER_BY_(DESC|ASC|UNSPECIFIED)$`,
		"limit":    `^([1-9]|[1-4][0-9]|50)$`,
		"page":     `^([1-9]|[1-9][0-9]|100)$`,
	}},

	// --- cosmos base ----------------------------------------------------
	{method: "GET", pattern: "/cosmos/base/node/v1beta1/config", grpc: "/cosmos.base.node.v1beta1.Service/Config", class: "light"},
	{method: "GET", pattern: "/cosmos/base/tendermint/v1beta1/blocks/latest", grpc: "/cosmos.base.tendermint.v1beta1.Service/GetLatestBlock", class: "light", maxResp: maxRespBlockLCD},
	{method: "GET", pattern: "/cosmos/base/tendermint/v1beta1/blocks/{uint}", grpc: "/cosmos.base.tendermint.v1beta1.Service/GetBlockByHeight", class: "light", maxResp: maxRespBlockLCD},
	{method: "GET", pattern: "/cosmos/base/tendermint/v1beta1/node_info", grpc: "/cosmos.base.tendermint.v1beta1.Service/GetNodeInfo", class: "light"},
	{method: "GET", pattern: "/cosmos/base/tendermint/v1beta1/syncing", grpc: "/cosmos.base.tendermint.v1beta1.Service/GetSyncing", class: "light"},
	// cosmpy (the backend's gas grants) reads the block gas limit here after
	// every simulate, to cap the gas it asks for: a fixed-size point read.
	{method: "GET", pattern: "/cosmos/consensus/v1/params", grpc: "/cosmos.consensus.v1.Query/Params", class: "light"},
	{method: "GET", pattern: "/cosmos/base/tendermint/v1beta1/validatorsets/latest", grpc: "/cosmos.base.tendermint.v1beta1.Service/GetLatestValidatorSet", page: true, class: "light"},

	// --- auth, bank -----------------------------------------------------
	{method: "GET", pattern: "/cosmos/auth/v1beta1/accounts/{addr}", grpc: "/cosmos.auth.v1beta1.Query/Account"},
	{method: "GET", pattern: "/cosmos/bank/v1beta1/balances/{addr}", grpc: "/cosmos.bank.v1beta1.Query/AllBalances", page: true},
	{method: "GET", pattern: "/cosmos/bank/v1beta1/balances/{addr}/by_denom", grpc: "/cosmos.bank.v1beta1.Query/Balance", params: map[string]string{"denom": reDenom}},
	{method: "GET", pattern: "/cosmos/bank/v1beta1/supply/by_denom", grpc: "/cosmos.bank.v1beta1.Query/SupplyOf", params: map[string]string{"denom": reDenom}},
	{method: "GET", pattern: "/cosmos/bank/v1beta1/send_enabled", grpc: "/cosmos.bank.v1beta1.Query/SendEnabled", page: true, params: map[string]string{"denoms": reDenom}},
	{method: "GET", pattern: "/cosmos/bank/v1beta1/params", grpc: "/cosmos.bank.v1beta1.Query/Params"},

	// --- staking, distribution, slashing --------------------------------
	{method: "GET", pattern: "/cosmos/staking/v1beta1/validators", grpc: "/cosmos.staking.v1beta1.Query/Validators", page: true, params: map[string]string{"status": reBondSt}},
	{method: "GET", pattern: "/cosmos/staking/v1beta1/validators/{addr}", grpc: "/cosmos.staking.v1beta1.Query/Validator"},
	{method: "GET", pattern: "/cosmos/staking/v1beta1/validators/{addr}/delegations/{addr}", grpc: "/cosmos.staking.v1beta1.Query/Delegation"},
	{method: "GET", pattern: "/cosmos/staking/v1beta1/validators/{addr}/delegations/{addr}/unbonding_delegation", grpc: "/cosmos.staking.v1beta1.Query/UnbondingDelegation"},
	{method: "GET", pattern: "/cosmos/staking/v1beta1/delegations/{addr}", grpc: "/cosmos.staking.v1beta1.Query/DelegatorDelegations", page: true},
	{method: "GET", pattern: "/cosmos/staking/v1beta1/delegators/{addr}/unbonding_delegations", grpc: "/cosmos.staking.v1beta1.Query/DelegatorUnbondingDelegations", page: true},
	{method: "GET", pattern: "/cosmos/staking/v1beta1/pool", grpc: "/cosmos.staking.v1beta1.Query/Pool"},
	{method: "GET", pattern: "/cosmos/staking/v1beta1/params", grpc: "/cosmos.staking.v1beta1.Query/Params"},
	{method: "GET", pattern: "/cosmos/distribution/v1beta1/delegators/{addr}/rewards", grpc: "/cosmos.distribution.v1beta1.Query/DelegationTotalRewards"},
	{method: "GET", pattern: "/cosmos/distribution/v1beta1/delegators/{addr}/rewards/{addr}", grpc: "/cosmos.distribution.v1beta1.Query/DelegationRewards"},
	{method: "GET", pattern: "/cosmos/distribution/v1beta1/validators/{addr}/commission", grpc: "/cosmos.distribution.v1beta1.Query/ValidatorCommission"},
	{method: "GET", pattern: "/cosmos/slashing/v1beta1/params", grpc: "/cosmos.slashing.v1beta1.Query/Params"},
	{method: "GET", pattern: "/cosmos/slashing/v1beta1/signing_infos", grpc: "/cosmos.slashing.v1beta1.Query/SigningInfos", page: true},

	// --- gov ------------------------------------------------------------
	{method: "GET", pattern: "/cosmos/gov/v1/proposals", grpc: "/cosmos.gov.v1.Query/Proposals", page: true},
	{method: "GET", pattern: "/cosmos/gov/v1/proposals/{uint}", grpc: "/cosmos.gov.v1.Query/Proposal"},
	{method: "GET", pattern: "/cosmos/gov/v1/proposals/{uint}/tally", grpc: "/cosmos.gov.v1.Query/TallyResult"},
	{method: "GET", pattern: "/cosmos/gov/v1/proposals/{uint}/votes/{addr}", grpc: "/cosmos.gov.v1.Query/Vote"},
	{method: "GET", pattern: "/cosmos/gov/v1/params/{name}", grpc: "/cosmos.gov.v1.Query/Params"},

	// --- earth ----------------------------------------------------------
	{method: "GET", pattern: "/earth/allocation/v1/params", grpc: "/earth.allocation.v1.Query/Params"},
	{method: "GET", pattern: "/earth/allocation/v1/options/{name}", grpc: "/earth.allocation.v1.Query/Options", page: true},
	{method: "GET", pattern: "/earth/allocation/v1/voter/{name}/{addr}", grpc: "/earth.allocation.v1.Query/Voter"},
	{method: "GET", pattern: "/earth/assembly/v1/proposal_tally/{uint}", grpc: "/earth.assembly.v1.Query/ProposalTally"},
	{method: "GET", pattern: "/earth/assembly/v1/removal_ballots", grpc: "/earth.assembly.v1.Query/RemovalBallots"},
	{method: "GET", pattern: "/earth/assembly/v1/ballot_inputs", grpc: "/earth.assembly.v1.Query/BallotInputs", params: map[string]string{"proposal_id": reUint, "option_id": reUint}},
	{method: "GET", pattern: "/earth/dex/v1/params", grpc: "/earth.dex.v1.Query/Params"},
	{method: "GET", pattern: "/earth/dex/v1/pool", grpc: "/earth.dex.v1.Query/ListPool", page: true},
	{method: "GET", pattern: "/earth/dex/v1/pool/{uint}", grpc: "/earth.dex.v1.Query/GetPool"},
	{method: "GET", pattern: "/earth/dex/v1/unbondings/{addr}", grpc: "/earth.dex.v1.Query/LpUnbondings"},
	{method: "GET", pattern: "/earth/dex/v1/liquidity_auction", grpc: "/earth.dex.v1.Query/LiquidityAuction"},
	{method: "GET", pattern: "/earth/dex/v1/liquidity_auction/bid/{addr}", grpc: "/earth.dex.v1.Query/AuctionBid"},
	{method: "GET", pattern: "/earth/dex/v1/pol_burns", grpc: "/earth.dex.v1.Query/PolBurns"},
	{method: "GET", pattern: "/earth/dex/v1/simulate_swap_exact_in", grpc: "/earth.dex.v1.Query/SimulateSwapExactIn", params: map[string]string{
		"offer_denom": reDenom, "offer_amount": `^[0-9]{1,40}$`, "ask_denom": reDenom}},
	{method: "GET", pattern: "/earth/earth/v1/burns", grpc: "/earth.earth.v1.Query/Burns"},
	{method: "GET", pattern: "/earth/personhood/v1/params", grpc: "/earth.personhood.v1.Query/Params"},
	{method: "GET", pattern: "/earth/personhood/v1/registration_count", grpc: "/earth.personhood.v1.Query/RegistrationCount"},
	{method: "GET", pattern: "/earth/personhood/v1/caretaker_voter_count", grpc: "/earth.personhood.v1.Query/CaretakerVoterCount"},
	{method: "GET", pattern: "/earth/personhood/v1/identity_tree", grpc: "/earth.personhood.v1.Query/IdentityTree"},
	{method: "GET", pattern: "/earth/personhood/v1/registration_countries", grpc: "/earth.personhood.v1.Query/RegistrationCountries"},
	{method: "GET", pattern: "/earth/personhood/v1/registrations_by_dsc/{hex}", grpc: "/earth.personhood.v1.Query/RegistrationsByDsc"},
	{method: "GET", pattern: "/earth/personhood/v1/lease_bounds", grpc: "/earth.personhood.v1.Query/LeaseBounds"},
	{method: "GET", pattern: "/earth/personhood/v1/handles", grpc: "/earth.personhood.v1.Query/Handles", params: map[string]string{"start": reText, "limit": reLimit}},
	{method: "GET", pattern: "/earth/shielded/v1/params", grpc: "/earth.shielded.v1.Query/Params"},
	{method: "GET", pattern: "/earth/shielded/v1/tree", grpc: "/earth.shielded.v1.Query/Tree"},
	{method: "GET", pattern: "/earth/shielded/v1/assets", grpc: "/earth.shielded.v1.Query/Assets", page: true},
	{method: "GET", pattern: "/earth/shielded/v1/turnstiles", grpc: "/earth.shielded.v1.Query/Turnstiles", page: true},
	{method: "GET", pattern: "/earth/shielded/v1/nullifiers/{hex}", grpc: "/earth.shielded.v1.Query/Nullifier"},
	{method: "GET", pattern: "/earth/shielded/v1/roots/{hex}", grpc: "/earth.shielded.v1.Query/Root"},
	{method: "GET", pattern: "/earth/shieldedstaking/v1/params", grpc: "/earth.shieldedstaking.v1.Query/Params"},
	{method: "GET", pattern: "/earth/shieldedstaking/v1/epoch", grpc: "/earth.shieldedstaking.v1.Query/Epoch"},
	{method: "GET", pattern: "/earth/shieldedstaking/v1/validators", grpc: "/earth.shieldedstaking.v1.Query/Validators", page: true},
	{method: "GET", pattern: "/earth/shieldedstaking/v1/positions", grpc: "/earth.shieldedstaking.v1.Query/Positions", page: true},
	{method: "GET", pattern: "/earth/shieldedstaking/v1/snapshots/{uint}", grpc: "/earth.shieldedstaking.v1.Query/Snapshot"},
	{method: "GET", pattern: "/earth/shieldedstaking/v1/stake_tree", grpc: "/earth.shieldedstaking.v1.Query/StakeTree"},
	{method: "GET", pattern: "/earth/shieldedstaking/v1/stake_nullifier_tree", grpc: "/earth.shieldedstaking.v1.Query/StakeNullifierTree", params: map[string]string{"start": reUint, "limit": reLimit}},
	{method: "GET", pattern: "/earth/shieldedstaking/v1/debt_tree", grpc: "/earth.shieldedstaking.v1.Query/DebtTree", params: map[string]string{"start": reUint, "limit": reLimit}},
	{method: "GET", pattern: "/earth/shieldedstaking/v1/stake_nullifiers/{hex}", grpc: "/earth.shieldedstaking.v1.Query/StakeNullifier"},
}

// The one search served: the txs of one block. CometBFT's kv indexer
// collects EVERY match of a search, loads each full TxResult and sorts them
// before it applies the page, and nothing cancels it (the SDK calls TxSearch
// with context.Background, and the local client ignores the connection). So
// a search costs what its match set costs, whatever `limit` says, and only
// a search whose match set is bounded may reach the node (round-5 R5-E-1):
//
//   - `tx.height=N` is the key prefix tx.height/N/N/: one block's txs.
//   - an address equality (`message.sender='…'`, `transfer.recipient='…'`)
//     is not bounded: the fee collector is the recipient of every fee, a
//     busy address of thousands of txs, and each is a whole-history scan.
//     Refused. (The validator also indexes no event attributes at all,
//     EARTHD_INDEX_EVENTS in akash/deploy.yaml, so such a search would find
//     nothing there anyway.) Wallets build their activity from the txs they
//     sent, looked up by hash.
//   - a range (`tx.height>0`) walks every tx.height entry: refused.
var searchQuery = `^tx\.height=[1-9][0-9]{0,18}$`

var lcdRoutes = buildLCDRoutes()

func buildLCDRoutes() []*lcdRoute {
	var out []*lcdRoute
	for _, s := range lcdSpecs {
		segs, err := compilePattern(s.pattern)
		if err != nil {
			panic("lcdpolicy: " + s.pattern)
		}
		rt := &lcdRoute{method: s.method, pattern: s.pattern, segs: segs,
			params: map[string]*regexp.Regexp{}, search: s.class == "search"}
		if s.page {
			for k, re := range paginationParams {
				rt.params[k] = re
			}
		}
		for k, v := range s.params {
			rt.params[k] = regexp.MustCompile(v)
		}
		if len(s.body) > 0 {
			rt.body = map[string]bool{}
			for _, k := range s.body {
				rt.body[k] = true
			}
		}
		switch s.class {
		case "light":
			rt.class = func(c *Classes) *class { return c.light }
		case "search":
			rt.class = func(c *Classes) *class { return c.search }
		case "bulk":
			rt.class = func(c *Classes) *class { return c.bulk }
		case "txhash":
			rt.class = func(c *Classes) *class { return c.txhash }
		case "broadcast":
			rt.class = func(c *Classes) *class { return c.broadcast }
		case "simulate":
			rt.class = func(c *Classes) *class { return c.simulate }
		case "":
			rt.class = func(c *Classes) *class { return c.query }
		default:
			panic("lcdpolicy: class " + s.class)
		}
		rt.grpc, rt.page, rt.maxResp = s.grpc, s.page, s.maxResp
		if s.shed != "" {
			if s.maxResp == 0 || !strings.HasSuffix(s.pattern, "/{hash}") {
				panic("lcdpolicy: shed needs a ceiling and a trailing {hash}: " + s.pattern)
			}
			rt.shed = s.shed
		}
		out = append(out, rt)
	}
	return out
}

// checkSearch: a search must say what it searches for (the deprecated
// repeated `events` is dropped by the filter, so it cannot stand in), and
// it is forwarded with an explicit page: absent, the SDK would use limit
// 100 (DefaultLimit), not the 50 served here (R5-E-4).
func checkSearch(q url.Values) error {
	if len(q["query"]) != 1 {
		return errors.New("tx search needs query=tx.height=N")
	}
	if q.Get("limit") == "" {
		q.Set("limit", "50")
	}
	if q.Get("page") == "" {
		q.Set("page", "1")
	}
	return nil
}

var searchRe = regexp.MustCompile(searchQuery)
