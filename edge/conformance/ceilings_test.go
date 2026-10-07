package conformance

import (
	"encoding/json"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/zenopie/earth-network-deploy/edge/filter"
)

// The chain's limits the edge's answer ceilings rest on are read from the
// pinned chain itself: its result caps from the source of app/resultcap (a
// package of constants, type-checked here so an expression like 1 << 20 is
// evaluated as the compiler would) and its block limits from
// networks/genesis.json. Nothing here is a copy of a chain number: a chain
// that renames, removes or changes one fails (round-8 R8-D-1, where a
// hard-coded 1 MiB passed against a chain that had dropped it).

// mempoolMaxTx: not the chain's but the node's, CometBFT's mempool
// max_tx_bytes default, which the SDL leaves alone (the edge's
// nodeMaxTxBytes).
const mempoolMaxTx = 1 << 20

// chainValues: every number the edge mirrors, by the names filter.ChainCaps
// uses: "resultcap.<Const>" and "genesis.<param>.<field>".
func chainValues(t *testing.T) map[string]int64 {
	t.Helper()
	chainGoMod(t) // .chain is the pinned commit
	out := map[string]int64{}

	dir := filepath.Join(chainDir, "app", "resultcap")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil || len(pkgs) != 1 {
		t.Fatalf("pinned chain's app/resultcap: %v (%d packages): run ./fetch-chain.sh", err, len(pkgs))
	}
	var files []*ast.File
	for _, p := range pkgs {
		for _, f := range p.Files {
			files = append(files, f)
		}
	}
	// No importer: the package is constants and has no imports. One that
	// grows imports fails here, to be looked at, not skipped.
	pkg, err := (&types.Config{}).Check("resultcap", fset, files, nil)
	if err != nil {
		t.Fatalf("type-checking the chain's app/resultcap: %v", err)
	}
	for _, name := range pkg.Scope().Names() {
		c, ok := pkg.Scope().Lookup(name).(*types.Const)
		if !ok || !c.Exported() {
			continue
		}
		v, exact := constant.Int64Val(constant.ToInt(c.Val()))
		if exact {
			out["resultcap."+name] = v
		}
	}

	raw, err := os.ReadFile(filepath.Join(chainDir, "networks", "genesis.json"))
	if err != nil {
		t.Fatalf("%v: run ./fetch-chain.sh", err)
	}
	var g struct {
		Consensus struct {
			Params map[string]map[string]json.RawMessage `json:"params"`
		} `json:"consensus"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	for _, k := range [][2]string{{"block", "max_bytes"}, {"block", "max_gas"}, {"evidence", "max_bytes"}} {
		var s string
		if err := json.Unmarshal(g.Consensus.Params[k[0]][k[1]], &s); err != nil {
			t.Fatalf("genesis consensus.params.%s.%s: %v", k[0], k[1], err)
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 {
			t.Fatalf("genesis consensus.params.%s.%s = %q: the ceilings assume a positive limit", k[0], k[1], s)
		}
		out["genesis."+k[0]+"."+k[1]] = v
	}
	return out
}

// TestChainCapsMatch: every chain limit the edge computes its ceilings from
// (edge/filter/chaincaps.go) is the pinned chain's, by name and value.
func TestChainCapsMatch(t *testing.T) {
	chain := chainValues(t)
	mirror := filter.ChainCaps()
	names := make([]string, 0, len(mirror))
	for k := range mirror {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v, ok := chain[k]
		switch {
		case !ok:
			t.Errorf("%s: not in the pinned chain (renamed or removed?): re-derive the edge's ceilings", k)
		case v != mirror[k]:
			t.Errorf("%s: the chain has %d, edge/filter/chaincaps.go %d: update it and re-derive the ceilings", k, v, mirror[k])
		default:
			t.Logf("%-28s %d", k, v)
		}
	}
	// Caps the chain added that the edge has not looked at.
	for k := range chain {
		if _, ok := mirror[k]; !ok && strings.HasPrefix(k, "resultcap.Max") {
			t.Logf("note: the chain also has %s = %d (not mirrored by the edge)", k, chain[k])
		}
	}
}

// TestAnswerCeilingsFitTheChain: each public answer ceiling (edge/filter
// forward.go) is above the largest answer the pinned chain allows, computed
// here from the chain's own numbers, relay txs included, and not so far
// above that it no longer binds.
func TestAnswerCeilingsFitTheChain(t *testing.T) {
	c := chainValues(t)
	need := func(k string) int64 {
		v, ok := c[k]
		if !ok {
			t.Fatalf("the pinned chain has no %s: the ceilings' arithmetic needs re-deriving", k)
		}
		return v
	}
	maxBlock, maxEvidence, maxGas := need("genesis.block.max_bytes"), need("genesis.evidence.max_bytes"), need("genesis.block.max_gas")
	txResult, freeBytes, gasPerByte, errBytes := need("resultcap.MaxTxResultBytes"), need("resultcap.FreeBytes"),
		need("resultcap.GasPerByte"), need("resultcap.MaxErrorBytes")
	endBlock, jsonX10 := need("resultcap.MaxEndBlockResultBytes"), need("resultcap.JSONPerCountedByteX10")
	ante, blockResultsMeasured := filter.Assumptions()
	tx, txLCD, blockResults, search, block, blockLCD := filter.AnswerCeilings()

	b64 := func(n int64) int64 { return (n + 2) / 3 * 4 }
	json := func(counted int64) int64 { return counted * jsonX10 / 10 }
	// The most a tx's result is counted: a non-relay tx's cap, or a relay
	// tx's free tier plus what the block's gas pays for; either with its
	// (uncounted) ante events and its log.
	ordinary := txResult + ante + errBytes
	relay := freeBytes + maxGas/gasPerByte + ante + errBytes
	result := json(max(ordinary, relay))
	// A block's results: the chain's measured worst (free-tier txs of tiny
	// attributes plus gov), which must at least cover what the constants
	// alone give: every paid byte of the block's gas, and gov's EndBlock.
	blockResultsBound := json(maxGas/gasPerByte + endBlock)
	if blockResultsMeasured < blockResultsBound {
		t.Errorf("edge assumes block_results JSON of at most %d, under the %d the chain's constants alone allow",
			blockResultsMeasured, blockResultsBound)
	}
	blockResultsMax := max(blockResultsMeasured, blockResultsBound)
	// A block in JSON: txs in base64, and evidence at up to 3x its proto
	// size (votes with base64 signatures and hex hashes), sharing max_bytes.
	blockJSON := b64(maxBlock-maxEvidence) + 3*maxEvidence + 64<<10
	for _, x := range []struct {
		what      string
		ceil, max int64
	}{
		{"RPC tx", tx, b64(mempoolMaxTx) + result},
		{"LCD txs/{hash}", txLCD, 2*2*mempoolMaxTx + result},
		{"RPC block", block, blockJSON},
		{"LCD block (block and sdk_block)", blockLCD, 2 * blockJSON},
		{"block_results", blockResults, blockResultsMax},
		{"LCD tx.height=N search", search, 4*maxBlock + blockResultsMax},
	} {
		t.Logf("%-32s ceiling %3d MiB, largest answer ~%.1f MiB", x.what, x.ceil>>20, float64(x.max)/(1<<20))
		if x.ceil < x.max {
			t.Errorf("%s: ceiling %d under the largest answer the chain allows (~%d)", x.what, x.ceil, x.max)
		}
		if x.ceil > 4*x.max {
			t.Errorf("%s: ceiling %d is over 4x the largest answer (~%d): lower it", x.what, x.ceil, x.max)
		}
	}
	t.Logf("tx result counted at most %d (non-relay %d, relay at block gas %d): ~%.1f MiB of JSON at %d/10 per counted byte",
		max(ordinary, relay), ordinary, maxGas, float64(result)/(1<<20), jsonX10)
}
