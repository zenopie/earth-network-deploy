package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/zenopie/earth-network-deploy/edge/filter"
)

// Constants of the chain the ceilings rest on, besides its genesis: the per-tx
// result cap (app/result_cap.go, 1 MiB) and a default node's mempool
// max_tx_bytes (1 MiB).
const (
	chainResultCap = 1 << 20
	mempoolMaxTx   = 1 << 20
	attrJSONPer10  = 46 // a one-byte event attribute's JSON over its proto size, x10 (4.6)
)

// TestAnswerCeilingsFitTheChain: each public answer ceiling (edge/filter
// forward.go) is above the largest answer the pinned chain allows, so the
// ceilings never cut an answer a block or tx within the chain's limits can
// produce, except block_results of a block built to be large (documented).
// A chain release that raises block max_bytes fails here, in the chain.pin
// bump, rather than as cut answers on the lease.
func TestAnswerCeilingsFitTheChain(t *testing.T) {
	chainGoMod(t) // .chain is the pinned commit
	raw, err := os.ReadFile(filepath.Join(chainDir, "networks", "genesis.json"))
	if err != nil {
		t.Fatalf("%v: run ./fetch-chain.sh", err)
	}
	var g struct {
		Consensus struct {
			Params struct {
				Block struct {
					MaxBytes string `json:"max_bytes"`
				} `json:"block"`
				Evidence struct {
					MaxBytes string `json:"max_bytes"`
				} `json:"evidence"`
			} `json:"params"`
		} `json:"consensus"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	maxBlock, err := strconv.ParseInt(g.Consensus.Params.Block.MaxBytes, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	maxEvidence, err := strconv.ParseInt(g.Consensus.Params.Evidence.MaxBytes, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	tx, txLCD, _, _, block, blockLCD := filter.AnswerCeilings()

	b64 := func(n int64) int64 { return (n + 2) / 3 * 4 }
	result := int64(attrJSONPer10 * chainResultCap / 10)
	// A block in JSON: txs in base64, and evidence at up to 3x its proto
	// size (votes with base64 signatures and hex hashes), sharing max_bytes.
	blockJSON := b64(maxBlock-maxEvidence) + 3*maxEvidence + 64<<10
	for _, c := range []struct {
		what      string
		ceil, max int64
	}{
		{"RPC tx", tx, b64(mempoolMaxTx) + result},
		{"LCD txs/{hash}", txLCD, 2*b64(mempoolMaxTx) + result},
		{"RPC block", block, blockJSON},
		{"LCD block (block and sdk_block)", blockLCD, 2 * blockJSON},
	} {
		t.Logf("%-32s ceiling %3d MiB, largest answer ~%.1f MiB", c.what, c.ceil>>20, float64(c.max)/(1<<20))
		if c.ceil < c.max {
			t.Errorf("%s: ceiling %d under the largest answer the chain allows (~%d) at block max_bytes %d",
				c.what, c.ceil, c.max, maxBlock)
		}
		// Not far above either: a ceiling is only a backstop if it binds.
		if c.ceil > 4*c.max {
			t.Errorf("%s: ceiling %d is over 4x the largest answer (~%d): lower it", c.what, c.ceil, c.max)
		}
	}
}
