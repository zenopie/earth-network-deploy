package filter

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"sync"
	"time"
)

// A class is a bound on concurrent upstream work of one kind. Each served
// request holds one slot of its class from the moment it is sent to the node
// until the node has answered it (forward.go): not until the client gives
// up. The node cannot be told to stop (CometBFT's abci_query and the SDK's
// queries ignore a dropped connection), so a slot freed at the client's
// deadline would let a new request in while the old one still runs, and the
// cap would not be a cap (round-5 R5-E-3). A request that cannot get a slot
// within the class's wait is answered 503.
//
// Per-client fairness (round-8 R8-D-1). A class's capacity is shared, so
// without a per-client bound one or two addresses could keep every slot of
// a class busy (Cloudflare's per-IP rate limit counts requests, not what
// they hold) and everyone else would wait out the class's wait and get 503.
// So each class also bounds what one client may have in it at once,
// holding a slot or queued for one: perClient. A request over that is
// answered 503 at once, without queueing. The slots are then granted in
// arrival order (one FIFO queue per class; Go's channel send order was not
// something to build on), so a waiting client is behind at most the
// requests already queued, of which every other client has at most
// perClient. In a class of 4 or more public slots two clients together
// hold at most half of them (DefaultClasses, held to it by a test); in the
// smallest classes (1 or 2 slots) a single client can hold a slot, and
// fairness is the queue: a third client's request is granted after at most
// the attackers' requests already queued, each one slot hold (a test).
// The client is the address Cloudflare names (client.go); the counters are
// keyed by a hash of it, held in memory only while that client has a
// request held or queued, deleted the moment its count reaches zero, and
// never logged (NO_LOGS.md).
//
// The one distinction besides the client is the backend: a request
// carrying the backend's credential (its Authorization header, checked
// against a SHA-256 the SDL holds, R5-E-7) is exempt from perClient (one
// address does every user's gas grants and the indexer) and may also use a
// few reserved slots in each class, so a public flood cannot starve gas
// grants and the indexer. A leaked credential buys only those slots and
// the public slots' share any client has.
type class struct {
	name      string
	wait      time.Duration // how long to queue for a slot
	timeout   time.Duration // the client's answer deadline (the slot outlives it)
	perClient int           // public requests one client may hold or queue

	mu      sync.Mutex
	pubCap  int // public slots
	resCap  int // backend-only slots
	pubUsed int
	resUsed int
	queue   []*waiter      // FIFO; no waiter waits while a slot it may use is free
	clients map[uint64]int // per client key: requests held or queued; no zero entries
}

type slotKind uint8

const (
	slotNone slotKind = iota
	slotPublic
	slotReserved
)

type waiter struct {
	backend bool
	ready   chan struct{} // closed when granted
	slot    slotKind      // set under mu when granted
}

func newClass(name string, public, reserved, perClient int, wait, timeout time.Duration) *class {
	return &class{name: name, pubCap: public, resCap: reserved, perClient: perClient,
		wait: wait, timeout: timeout, clients: map[uint64]int{}}
}

// acquire takes a slot and returns its release, or nil if the client is at
// its perClient bound or no slot came free within the wait. The backend
// tries its reserve first, then the public slots, and has no per-client
// bound; client is ignored for it.
func (c *class) acquire(ctx context.Context, backend bool, client uint64) func() {
	c.mu.Lock()
	if !backend {
		if c.clients[client] >= c.perClient {
			c.mu.Unlock()
			return nil
		}
		c.clients[client]++
	}
	// The queue holds no waiter that a free slot could serve (dispatch runs
	// on every release), so a free slot here jumps no one.
	if backend && c.resUsed < c.resCap {
		c.resUsed++
		c.mu.Unlock()
		return c.releaser(slotReserved, backend, client)
	}
	if c.pubUsed < c.pubCap {
		c.pubUsed++
		c.mu.Unlock()
		return c.releaser(slotPublic, backend, client)
	}
	if c.wait <= 0 {
		c.forget(backend, client)
		c.mu.Unlock()
		return nil
	}
	w := &waiter{backend: backend, ready: make(chan struct{})}
	c.queue = append(c.queue, w)
	c.mu.Unlock()

	t := time.NewTimer(c.wait)
	defer t.Stop()
	select {
	case <-w.ready:
		return c.releaser(w.slot, backend, client)
	case <-t.C:
	case <-ctx.Done():
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if w.slot != slotNone { // granted as the wait ended
		return c.releaser(w.slot, backend, client)
	}
	for i, q := range c.queue {
		if q == w {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			break
		}
	}
	c.forget(backend, client)
	return nil
}

// forget drops one of a public client's requests from its count, and the
// client's entry with its last one. Under c.mu.
func (c *class) forget(backend bool, client uint64) {
	if backend {
		return
	}
	if n := c.clients[client] - 1; n > 0 {
		c.clients[client] = n
	} else {
		delete(c.clients, client)
	}
}

// releaser frees the slot (once) and hands it to the first waiter that may
// use it.
func (c *class) releaser(kind slotKind, backend bool, client uint64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if kind == slotReserved {
				c.resUsed--
			} else {
				c.pubUsed--
			}
			c.forget(backend, client)
			c.dispatch()
		})
	}
}

// dispatch grants free slots to waiters in arrival order: a reserved slot
// to the first backend waiter, a public slot to the first waiter. Under c.mu.
func (c *class) dispatch() {
	for i := 0; i < len(c.queue); {
		w := c.queue[i]
		switch {
		case w.backend && c.resUsed < c.resCap:
			c.resUsed++
			w.slot = slotReserved
		case c.pubUsed < c.pubCap:
			c.pubUsed++
			w.slot = slotPublic
		default:
			if c.pubUsed >= c.pubCap && c.resUsed >= c.resCap {
				return
			}
			i++
			continue
		}
		c.queue = append(c.queue[:i], c.queue[i+1:]...)
		close(w.ready)
	}
}

// inUse and caps report the slots (tests).
func (c *class) inUse() (public, reserved int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pubUsed, c.resUsed
}

func (c *class) caps() (public, reserved int) { return c.pubCap, c.resCap }

// clientCount reports how many clients the class holds counters for (tests:
// zero once idle).
func (c *class) clientCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.clients)
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
// node's build of one answer: what bounds the build is the chain's result
// caps (chaincaps.go: 1 MiB of msg results for a tx with any non-relay msg;
// a relay tx only by gas, ~5 MB at the block's 100M; ~10 MB per block).
// What keeps those builds few: the class sizes, the per-client bound (one
// address holds at most 1 bulk and 2 txhash slots), and shedding (shed.go:
// an answer once found over its ceiling is not built again for the public).
//
//   - bulk: block_results. One block's results, up to ~10 MB of proto for
//     a block built to be large: few at a time, and the backend (the
//     indexer) keeps a slot.
//   - txhash: one tx by hash (RPC tx, LCD txs/{hash}). It is what every
//     wallet's commit poll and activity refresh calls, almost always a
//     millisecond point read. It has its own slots so that anonymous
//     block_results reads, however heavy, cannot make a committed tx look
//     unconfirmed (round-7 R7-D-1). A tx built to be heavy (1 MiB of results
//     for ~21M gas, ~0.1 ERTH) still costs a build per request, so one
//     address may hold only 2 of the 8 slots; a relay tx heavier than the
//     ceiling (up to ~5 MB of results for 100M gas, ~0.5 ERTH) is built
//     once per shedTTL per form (RPC, RPC with prove, LCD) for everyone
//     together.
type Classes struct {
	light     *class // point reads that do not touch the app: status, a block, a commit
	results   *class // block ranges (headers), genesis_chunked: larger bodies of fixed size
	bulk      *class // block_results: one block's results, sized by chain data (above)
	txhash    *class // RPC tx, LCD txs/{hash}: one tx, sized by the chain's result caps
	query     *class // LCD gRPC GETs: run outside the ABCI mutex, metered by query gas
	abciQuery *class // RPC abci_query, abci_info: under the ABCI mutex
	broadcast *class // RPC broadcast_tx_sync/async, LCD POST txs (CheckTx): under the ABCI mutex
	simulate  *class // LCD simulate: CPU (proofs, contract code), outside the mutex
	search    *class // the LCD tx search, tx.height=N only: one block's txs

	oversized *shedList // answers known to be over their ceiling (shed.go)

	backendAuth []byte // SHA-256 of the backend's exact Authorization value; nil: no backend
}

// abciMutexHolders: the most requests the edge has inside (or queued on)
// the node's ABCI mutex at once, public and backend slots of abciQuery and
// broadcast together. A test holds DefaultClasses to it.
const abciMutexHolders = 4

func DefaultClasses() *Classes {
	w := 2 * time.Second
	// perClient: two clients together hold at most half of any class of 4
	// or more public slots; the classes of 1 or 2 slots give a client one
	// and rely on the queue (above). TestPerClientBounds holds them to it.
	//
	// txhash: a commit poll is a point read in the tx index, milliseconds
	// for an ordinary tx, so 8 slots serve hundreds of polls a second (a
	// phone polls at most 20 times per tx, and the activity refresh a few
	// hashes at a time). A tx's answer is sized by what it stored, which
	// the chain bounds (forward.go, the answer ceilings); a client asking
	// for the heaviest txs holds at most 2 of the 8, an answer once found
	// over its ceiling is refused without asking the node again (shed.go),
	// and block_results does not compete for these slots. Every slot
	// together is 64, the transport's MaxConnsPerHost
	// (TestSlotsFitConnections); light gave up the slots txhash needed (its
	// calls do not touch the app).
	return &Classes{
		light:     newClass("light", 12, 3, 3, w, 15*time.Second),
		results:   newClass("results", 8, 4, 2, w, 30*time.Second),
		bulk:      newClass("bulk", 2, 1, 1, w, 30*time.Second),
		txhash:    newClass("txhash", 8, 2, 2, w, 20*time.Second),
		query:     newClass("query", 12, 4, 3, w, 20*time.Second),
		abciQuery: newClass("abci-query", 1, 1, 1, 4*time.Second, 20*time.Second),
		broadcast: newClass("broadcast", 1, 1, 1, 4*time.Second, 20*time.Second),
		simulate:  newClass("simulate", 2, 1, 1, 4*time.Second, 30*time.Second),
		search:    newClass("search", 1, 0, 1, w, 10*time.Second),
		oversized: newShedList(shedTTL, shedMax),
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
