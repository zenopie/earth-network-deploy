package filter

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"time"
)

// A class is a bound on concurrent upstream work of one kind. Each served
// request holds one slot of its class from the moment it is sent to the node
// until the node has answered it (forward.go): not until the client gives
// up. The node cannot be told to stop (CometBFT's abci_query and the SDK's
// queries ignore a dropped connection), so a slot freed at the client's
// deadline would let a new request in while the old one still runs, and the
// cap would not be a cap (round-5 R5-E-3). A request that cannot get a slot
// within the class's wait is answered 503 at once.
//
// There is no per-client state here. Per-address limits are Cloudflare's
// (akash/README.md); this process never sees a client's
// address except as a header it drops. The one distinction it makes is the
// backend: a request carrying the backend's credential (its Authorization
// header, checked against a SHA-256 the SDL holds, R5-E-7) may also use a
// few reserved slots in each class, so a public flood cannot starve gas
// grants and the indexer. A leaked credential buys only those slots.
type class struct {
	name     string
	sem      chan struct{} // public slots
	reserved chan struct{} // backend-only slots (nil: none)
	wait     time.Duration // how long to queue for a slot
	timeout  time.Duration // the client's answer deadline (the slot outlives it)
}

func newClass(name string, public, reserved int, wait, timeout time.Duration) *class {
	c := &class{name: name, sem: make(chan struct{}, public), wait: wait, timeout: timeout}
	if reserved > 0 {
		c.reserved = make(chan struct{}, reserved)
	}
	return c
}

// acquire takes a slot and returns its release, or nil if none came free in
// time. The backend tries its reserve first, then the public slots.
func (c *class) acquire(ctx context.Context, backend bool) func() {
	pub := func() { <-c.sem }
	if backend && c.reserved != nil {
		res := func() { <-c.reserved }
		select {
		case c.reserved <- struct{}{}:
			return res
		default:
		}
		select {
		case c.sem <- struct{}{}:
			return pub
		default:
		}
		t := time.NewTimer(c.wait)
		defer t.Stop()
		select {
		case c.reserved <- struct{}{}:
			return res
		case c.sem <- struct{}{}:
			return pub
		case <-t.C:
		case <-ctx.Done():
		}
		return nil
	}
	select {
	case c.sem <- struct{}{}:
		return pub
	default:
	}
	t := time.NewTimer(c.wait)
	defer t.Stop()
	select {
	case c.sem <- struct{}{}:
		return pub
	case <-t.C:
	case <-ctx.Done():
	}
	return nil
}

// holdCeiling: the longest a slot stays held for one upstream request. The
// node answers well inside it (query gas, block gas, a bounded search); a
// request still unanswered then means the node is wedged, and the request is
// abandoned so the class does not stay full for ever.
const holdCeiling = 3 * time.Minute

// Classes. Capacities are per process (one proxy per lease) and sum to well
// under the node's own connection caps (EARTHD_RPC_MAX_OPEN_CONNECTIONS 100,
// EARTHD_API_MAX_OPEN_CONNECTIONS 200), so the node never refuses a
// connection the proxy made and the proxy's 503 is the only overload answer.
//
// The abci class is the node's real bottleneck. CometBFT runs the app behind
// one mutex (the SDK starts it with a local client creator; every ABCI
// connection shares it), and consensus (PrepareProposal, ProcessProposal,
// FinalizeBlock, Commit) queues on it behind whatever holds it. Everything
// the public endpoints send through that mutex is in this one class: RPC
// abci_query (every path), broadcast_tx_sync/async and the LCD broadcast
// (CheckTx), and abci_info. With at most 2 public + 1 backend holders, each
// bounded (a query by query-gas-limit, a CheckTx by its proofs and the block
// gas limit), a block step waits behind at most three of them.
type Classes struct {
	light    *class // point reads that do not touch the app: status, a block, a commit, a tx by hash
	results  *class // block_results, block ranges, genesis_chunked: larger bodies
	query    *class // LCD gRPC GETs: run outside the ABCI mutex, metered by query gas
	abci     *class // everything that takes CometBFT's ABCI mutex (see above)
	simulate *class // LCD simulate: CPU (proofs, contract code), outside the mutex
	search   *class // the LCD tx search, tx.height=N only: one block's txs

	backendAuth []byte // SHA-256 of the backend's exact Authorization value; nil: no backend
}

func DefaultClasses() *Classes {
	w := 2 * time.Second
	return &Classes{
		light:    newClass("light", 24, 4, w, 15*time.Second),
		results:  newClass("results", 8, 4, w, 30*time.Second),
		query:    newClass("query", 12, 4, w, 20*time.Second),
		abci:     newClass("abci", 2, 1, 4*time.Second, 20*time.Second),
		simulate: newClass("simulate", 2, 1, 4*time.Second, 30*time.Second),
		search:   newClass("search", 1, 0, w, 10*time.Second),
	}
}

// SetBackendAuthSHA256 names the backend: a request whose Authorization
// header hashes to h may use the reserved slots. The header itself is still
// never forwarded.
func (c *Classes) SetBackendAuthSHA256(h []byte) {
	if len(h) == sha256.Size {
		c.backendAuth = append([]byte(nil), h...)
	}
}

func (c *Classes) isBackend(r *http.Request) bool {
	if c.backendAuth == nil {
		return false
	}
	v := r.Header.Get("Authorization")
	if v == "" || len(v) > 512 {
		return false
	}
	sum := sha256.Sum256([]byte(v))
	return subtle.ConstantTimeCompare(sum[:], c.backendAuth) == 1
}
