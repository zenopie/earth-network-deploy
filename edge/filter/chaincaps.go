package filter

// The chain's limits the answer ceilings (forward.go) are computed from.
// Each mirrors one value of the chain this edge was checked against
// (edge/conformance/chain.pin): edge/conformance reads the pinned chain's
// source and genesis and fails on any difference, and on a constant that
// was renamed or removed, so a chain release that changes a cap cannot pass
// conformance with these numbers stale (round-8 R8-D-1: a hard-coded
// 1 MiB in the test once passed against a chain that no longer had it).
//
// chainCaps names each one as conformance finds it: "resultcap.X" is
// constant X of the chain's app/resultcap package; "genesis.X" a consensus
// param of networks/genesis.json.
const (
	// A tx's msg results, counted at their worst-case JSON size, unless
	// every msg is a relay msg (resultcap).
	chainMaxTxResultBytes = 1 << 20
	// The free per-tx result allowance, and the gas per byte past it: a
	// relay tx's result is bounded only by FreeBytes + gas / GasPerByte.
	chainFreeBytes  = 8 << 10
	chainGasPerByte = 20
	// The cap on an error text that reaches a result (a failed tx's log).
	chainMaxErrorBytes = 1 << 10
	// JSON bytes per counted result byte, x10: the chain counts escapes at
	// their JSON size, so only JSON structure is left over (3.3x at worst).
	chainJSONPerCountedByteX10 = 33
	// The most bytes one byte of a string becomes in a node's JSON (\u003c
	// for '<'): the chain's count for event strings, and the factor for a
	// tx's own strings in the LCD's tx answers (audit R9-D-1).
	chainJSONEscapeBytes = 6
	// Consensus params (genesis): block bytes, evidence bytes within them,
	// and block gas, which bounds a relay tx's and a block's paid results.
	chainBlockMaxBytes    = 4 << 20
	chainEvidenceMaxBytes = 1 << 20
	chainBlockMaxGas      = 100_000_000
)

// Not the chain's but the node's: CometBFT's mempool max_tx_bytes default,
// which the SDL does not change, bounds the txs this validator (the only
// proposer) admits.
const nodeMaxTxBytes = 1 << 20

// chainCaps: each mirrored value by the name conformance reads it under.
var chainCaps = map[string]int64{
	"resultcap.MaxTxResultBytes":      chainMaxTxResultBytes,
	"resultcap.FreeBytes":             chainFreeBytes,
	"resultcap.GasPerByte":            chainGasPerByte,
	"resultcap.MaxErrorBytes":         chainMaxErrorBytes,
	"resultcap.JSONPerCountedByteX10": chainJSONPerCountedByteX10,
	"resultcap.JSONEscapeBytes":       chainJSONEscapeBytes,
	"genesis.block.max_bytes":         chainBlockMaxBytes,
	"genesis.evidence.max_bytes":      chainEvidenceMaxBytes,
	"genesis.block.max_gas":           chainBlockMaxGas,
}
