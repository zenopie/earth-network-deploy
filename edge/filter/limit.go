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
// They also sum to under the transport's MaxConnsPerHost (64), so a slot
// holder never waits for a connection (a test holds them to it).
//
// The ABCI mutex is the node's real bottleneck. CometBFT runs the app behind
// one mutex (the SDK starts it with a local client creator; every ABCI
// connection shares it), and consensus (PrepareProposal, ProcessProposal,
// FinalizeBlock, Commit) queues on it behind whatever holds it. Everything
// the public endpoints send through that mutex is in one of two classes
// whose slots together are abciMutexHolders: abciQuery (RPC abci_query on
// every path, abci_info) and broadcast (broadcast_tx_sync/async and the LCD
// broadcast, i.e. CheckTx). Each holder is bounded (a query by
// query-gas-limit, a CheckTx by its proofs and the block gas limit), so a
// block step waits behind at most abciMutexHolders of them. They are split
// by purpose so a flood of anonymous reads cannot starve tx submission
// (round-6 R6-E-3), and each keeps a backend slot (gas-check's store reads,
// the backend's own broadcasts).
//
// Two classes hold the calls whose answer size is set by what was put on
// chain rather than by the request. The node builds each such answer in
// memory several times over (round-6 R6-E-1 measured ~6x), and each also
// has a byte ceiling (forward.go, fwdOpts.maxResp). Neither stops the
// node's build of one answer: what bounds the build is the chain's per-tx
// result cap (app/result_cap.go: 1 MiB per tx, about 10 MB per block).
//
//   - bulk: block_results. One block's results, up to ~10 MB of proto for
//     a block built to be large: few at a time, and the backend (the
//     indexer) keeps a slot.
//   - txhash: one tx by hash (RPC tx, LCD txs/{hash}). Its answer is
//     bounded by the per-tx cap (a few MiB of JSON at worst, forward.go),
//     so it does not need block_results' tiny class, and it is what every
//     wallet's commit poll and activity refresh calls. It has its own
//     slots so that anonymous block_results reads, however heavy, cannot
//     make a committed tx look unconfirmed (round-7 R7-D-1).
type Classes struct {
	light     *class // point reads that do not touch the app: status, a block, a commit
	results   *class // block ranges (headers), genesis_chunked: larger bodies of fixed size
	bulk      *class // block_results: one block's results, sized by chain data (above)
	txhash    *class // RPC tx, LCD txs/{hash}: one tx, bounded by the per-tx result cap
	query     *class // LCD gRPC GETs: run outside the ABCI mutex, metered by query gas
	abciQuery *class // RPC abci_query, abci_info: under the ABCI mutex
	broadcast *class // RPC broadcast_tx_sync/async, LCD POST txs (CheckTx): under the ABCI mutex
	simulate  *class // LCD simulate: CPU (proofs, contract code), outside the mutex
	search    *class // the LCD tx search, tx.height=N only: one block's txs

	backendAuth []byte // SHA-256 of the backend's exact Authorization value; nil: no backend
}

// abciMutexHolders: the most requests the edge has inside (or queued on)
// the node's ABCI mutex at once, public and backend slots of abciQuery and
// broadcast together. A test holds DefaultClasses to it.
const abciMutexHolders = 4

func DefaultClasses() *Classes {
	w := 2 * time.Second
	// txhash: a commit poll is a point read in the tx index, milliseconds
	// for an ordinary tx, so 8 slots serve hundreds of polls a second (a
	// phone polls at most 20 times per tx, and the activity refresh a few
	// hashes at a time). An attacker who wants them busy has to ask for txs
	// near the 1 MiB result cap, each paid for and each a bounded build;
	// block_results no longer competes for them. Every slot together is
	// 64, the transport's MaxConnsPerHost (TestSlotsFitConnections); light
	// gave up the slots txhash needed (its calls do not touch the app).
	return &Classes{
		light:     newClass("light", 12, 3, w, 15*time.Second),
		results:   newClass("results", 8, 4, w, 30*time.Second),
		bulk:      newClass("bulk", 2, 1, w, 30*time.Second),
		txhash:    newClass("txhash", 8, 2, w, 20*time.Second),
		query:     newClass("query", 12, 4, w, 20*time.Second),
		abciQuery: newClass("abci-query", 1, 1, 4*time.Second, 20*time.Second),
		broadcast: newClass("broadcast", 1, 1, 4*time.Second, 20*time.Second),
		simulate:  newClass("simulate", 2, 1, 4*time.Second, 30*time.Second),
		search:    newClass("search", 1, 0, w, 10*time.Second),
	}
}

// all lists every class (tests, and the ABCI holder count).
func (c *Classes) all() []*class {
	return []*class{c.light, c.results, c.bulk, c.txhash, c.query, c.abciQuery, c.broadcast, c.simulate, c.search}
}

// isABCI: the class's requests take the node's ABCI mutex.
func (c *Classes) isABCI(cl *class) bool { return cl == c.abciQuery || cl == c.broadcast }

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
