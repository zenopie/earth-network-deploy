package filter

// What rpc.erth.network serves, and to whom (the client inventory is in
// akash/README.md, "What the clients call"):
//
//   wallets, web app, backend indexer   GET status, blockchain, block,
//                                       block_results, genesis_chunked,
//                                       abci_query (gRPC Query paths)
//   earthd --node (docs, runbook)       JSON-RPC POST: status, block, tx,
//                                       abci_query (the gRPC paths below),
//                                       broadcast_tx_sync (with an explicit
//                                       --gas: Simulate is not served here)
//   earthd gas-check (backend)          JSON-RPC POST: status, block,
//                                       abci_query /store/<m>/key (prove) and
//                                       narrow /store/<m>/subspace reads
//   state sync light client             JSON-RPC POST: commit, validators,
//                                       consensus_params
//
// Refused: tx_search and block_search (unmetered kv-index scans; nothing of
// ours needs them: wallets look a tx up by hash, the explorer searches
// through the LCD under its own checks), /websocket, subscriptions,
// broadcast_tx_commit, the mempool and consensus dumps, net_info, genesis
// (genesis_chunked is the same data in pieces), check_tx,
// broadcast_evidence, the unsafe routes, and any route a future CometBFT
// adds.

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

var rpcRoutes = map[string]rpcRoute{
	"health":             {},
	"status":             {},
	"abci_info":          {},
	"genesis_chunked":    {args: []rpcArg{{"chunk", KUint}}},
	"block":              {args: []rpcArg{{"height", KInt64Ptr}}},
	"block_results":      {args: []rpcArg{{"height", KInt64Ptr}}},
	"commit":             {args: []rpcArg{{"height", KInt64Ptr}}},
	"consensus_params":   {args: []rpcArg{{"height", KInt64Ptr}}},
	"validators":         {args: []rpcArg{{"height", KInt64Ptr}, {"page", KIntPtr}, {"per_page", KIntPtr}}},
	"blockchain":         {args: []rpcArg{{"minHeight", KInt64}, {"maxHeight", KInt64}}},
	"tx":                 {args: []rpcArg{{"hash", KBytes}, {"prove", KBool}}},
	"abci_query":         {args: []rpcArg{{"path", KString}, {"data", KHexBytes}, {"height", KInt64}, {"prove", KBool}}},
	"broadcast_tx_sync":  {args: []rpcArg{{"tx", KBytes}}},
	"broadcast_tx_async": {args: []rpcArg{{"tx", KBytes}}},
}

var errRefused = errors.New("refused")

// checkRPC decides whether a decoded call is served, and in which class.
func checkRPC(c *Classes, call rpcCall) (*class, error) {
	a := call.args
	switch call.method {
	case "health", "status", "block", "commit", "consensus_params":
		return c.light, nil
	case "abci_info":
		// Cheap, but it takes the ABCI mutex (localClient.Info).
		return c.abciQuery, nil
	case "blockchain", "genesis_chunked":
		return c.results, nil
	case "block_results":
		return c.bulk, nil
	case "validators":
		if v := a["per_page"]; v.set && (v.i < 1 || v.i > 100) {
			return nil, fmt.Errorf("%w: per_page must be 1..100", errRefused)
		}
		if v := a["page"]; v.set && (v.i < 1 || v.i > 1000) {
			return nil, fmt.Errorf("%w: page must be 1..1000", errRefused)
		}
		return c.light, nil
	case "tx":
		if len(a["hash"].b) != 32 {
			return nil, fmt.Errorf("%w: hash must be 32 bytes", errRefused)
		}
		return c.txhash, nil
	case "broadcast_tx_sync", "broadcast_tx_async":
		if len(a["tx"].b) == 0 {
			return nil, fmt.Errorf("%w: empty tx", errRefused)
		}
		return c.broadcast, nil
	case "abci_query":
		return checkABCIQuery(c, a["path"].s, a["data"].b, a["prove"].t)
	}
	return nil, fmt.Errorf("%w: method not served", errRefused)
}

// abciGRPC: the gRPC paths served over abci_query, each named for the
// client that sends it. An explicit list, not a mirror of the LCD: every
// call here holds the node's ABCI mutex, which consensus waits on, so only
// what a client needs is served, and only methods whose request has no
// PageRequest (each is a point read, a fixed-size answer, or a page its
// keeper caps). conformance/ checks the PageRequest part against the protos.
// The LCD serves everything else outside the mutex. Not served:
// cosmos.tx.v1beta1.Service/Simulate (round-5 R5-E-2: a simulate runs the
// whole tx inside the mutex; `earthd tx` against the public RPC passes an
// explicit --gas, and wallets simulate through the LCD), and every other
// cosmos.tx method (GetTxsEvent is the tx search).
var abciGRPC = map[string]bool{
	// earthd tx: the signer's account number and sequence.
	"/cosmos.auth.v1beta1.Query/Account": true,
	// The trust-store runbook (DSC revocation).
	"/cosmos.auth.v1beta1.Query/ModuleAccountByName": true,
	"/cosmos.gov.v1.Query/Proposal":                  true,
	"/earth.personhood.v1.Query/Registration":        true,
	"/earth.personhood.v1.Query/RegistrationsByDsc":  true,
	"/earth.assembly.v1.Query/ProposalTally":         true,
	// The web app's issuance figure (explorer, SupplyOf at height 1).
	"/cosmos.bank.v1beta1.Query/SupplyOf": true,
	// The backend indexer and tree verifier, at a height. Handles,
	// StakeNullifierTree and DebtTree take their own start/limit, capped by
	// their keepers (HandleQueryMaxLimit, 1000).
	"/earth.shielded.v1.Query/Tree":                      true,
	"/earth.personhood.v1.Query/IdentityTree":            true,
	"/earth.personhood.v1.Query/Handles":                 true,
	"/earth.shieldedstaking.v1.Query/StakeTree":          true,
	"/earth.shieldedstaking.v1.Query/StakeNullifierTree": true,
	"/earth.shieldedstaking.v1.Query/DebtTree":           true,
}

// Raw store reads. These do not run under the query-gas meter, so each is
// pinned to what earthd gas-check and the backend's known-DSC refresh read:
// point reads (with a proof when the value is empty) in the three stores
// gas-check opens, and prefix reads that stay inside one small set.
var storeKeyModules = map[string]bool{"personhood": true, "pki": true, "shielded": true}

// subspaceRule: a prefix read in store is served when the prefix starts
// with collection and is at least min bytes long. gas-check iterates CSCA
// index ranges (x/pki keeper issuerCandidates: csca_by_ski + AKI, csca_by_dn
// + sha256(DN); the prefix it sends is the range's common prefix, i.e. the
// collection prefix plus almost all of the key); the backend reads the whole
// regs_by_dsc collection (one entry per Document Signer with live
// registrations).
type subspaceRule struct {
	store, collection string
	min               int
}

var subspaceRules = []subspaceRule{
	{"pki", "csca_by_ski", len("csca_by_ski") + 4},
	{"pki", "csca_by_dn", len("csca_by_dn") + 4},
	{"personhood", "regs_by_dsc", len("regs_by_dsc")},
}

const maxStoreKey = 512

func checkABCIQuery(c *Classes, path string, data []byte, prove bool) (*class, error) {
	// The node routes a path that is a registered gRPC method to it, and
	// otherwise splits on "/" (baseapp Query). Matching the decoded string
	// exactly against fixed strings leaves no second reading.
	if abciGRPC[path] {
		if prove {
			return nil, fmt.Errorf("%w: prove is not served on gRPC paths", errRefused)
		}
		return c.abciQuery, nil
	}
	if !strings.HasPrefix(path, "/store/") {
		return nil, fmt.Errorf("%w: abci_query path not served", errRefused)
	}
	parts := strings.Split(path, "/") // "", "store", module, kind
	if len(parts) != 4 {
		return nil, fmt.Errorf("%w: abci_query path not served", errRefused)
	}
	module, kind := parts[2], parts[3]
	switch kind {
	case "key":
		if !storeKeyModules[module] {
			return nil, fmt.Errorf("%w: store not served", errRefused)
		}
		if len(data) == 0 || len(data) > maxStoreKey {
			return nil, fmt.Errorf("%w: key must be 1..%d bytes", errRefused, maxStoreKey)
		}
		return c.abciQuery, nil
	case "subspace":
		if prove {
			return nil, fmt.Errorf("%w: prove is not served on subspace reads", errRefused)
		}
		for _, r := range subspaceRules {
			if r.store == module && bytes.HasPrefix(data, []byte(r.collection)) &&
				len(data) >= r.min && len(data) <= maxStoreKey {
				return c.abciQuery, nil
			}
		}
		return nil, fmt.Errorf("%w: subspace prefix not served", errRefused)
	}
	return nil, fmt.Errorf("%w: abci_query path not served", errRefused)
}
