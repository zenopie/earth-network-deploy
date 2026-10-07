package filter

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func reqFrom(method, target, ip string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	if ip != "" {
		r.Header.Set(clientIPHeader, ip)
	}
	return r
}

// R8-D-1: the client is Cloudflare's address for it, IPv6 by /64; anything
// but one clean header value falls back to the TCP peer (cloudflared), so a
// bad header buys no fresh allowance.
func TestClientKey(t *testing.T) {
	k := func(ip string) uint64 { return clientKey(reqFrom("GET", "/status", ip)) }
	same := [][2]string{
		{"2001:db8:1:2::1", "2001:db8:1:2:ffff:ffff:ffff:ffff"}, // one /64
		{"203.0.113.9", "::ffff:203.0.113.9"},                   // mapped
		{"203.0.113.9", " 203.0.113.9 "},
		{"", "not an address"},  // both the peer
		{"", "203.0.113.9:443"}, // not a bare address: the peer
		{"", "fe80::1%eth0"},    // zoned: the peer
	}
	for _, p := range same {
		if k(p[0]) != k(p[1]) {
			t.Errorf("%q and %q are different clients", p[0], p[1])
		}
	}
	differ := [][2]string{
		{"2001:db8:1:2::1", "2001:db8:1:3::1"},
		{"203.0.113.9", "203.0.113.10"},
		{"203.0.113.9", ""},
		{"192.0.2.1", ""}, // the header's address is not the peer's, even when equal
	}
	for _, p := range differ {
		if k(p[0]) == k(p[1]) {
			t.Errorf("%q and %q are one client", p[0], p[1])
		}
	}
	// Two values: the peer, not either value.
	r := reqFrom("GET", "/status", "203.0.113.9")
	r.Header.Add(clientIPHeader, "203.0.113.10")
	if clientKey(r) != k("") {
		t.Error("two CF-Connecting-IP values not treated as none")
	}
	// Different peers without the header are different clients.
	r1, r2 := reqFrom("GET", "/status", ""), reqFrom("GET", "/status", "")
	r2.RemoteAddr = "192.0.2.2:1234"
	if clientKey(r1) == clientKey(r2) {
		t.Error("two peers share a key")
	}
}

// Two clients together hold at most half of a class of 4 or more public
// slots; the smaller classes give a client one slot (the queue is their
// fairness).
func TestPerClientBounds(t *testing.T) {
	for _, cl := range DefaultClasses().all() {
		pub, _ := cl.caps()
		switch {
		case cl.perClient < 1:
			t.Errorf("%s: perClient %d", cl.name, cl.perClient)
		case pub >= 4 && 2*cl.perClient > pub/2:
			t.Errorf("%s: two clients hold %d of %d public slots", cl.name, 2*cl.perClient, pub)
		case pub < 4 && cl.perClient != 1:
			t.Errorf("%s: %d public slots, perClient %d", cl.name, pub, cl.perClient)
		}
	}
}

// slowNode answers every request after hold, counting how many it works on
// at once.
type slowNode struct {
	srv           *httptest.Server
	inFlight, max atomic.Int32
}

func newSlowNode(t *testing.T, hold time.Duration) *slowNode {
	n := &slowNode{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c := n.inFlight.Add(1)
		for {
			m := n.max.Load()
			if c <= m || n.max.CompareAndSwap(m, c) {
				break
			}
		}
		time.Sleep(hold)
		n.inFlight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(n.srv.Close)
	return n
}

type classCase struct {
	name   string
	method string
	target string
	body   string
	pick   func(*Classes) *class
}

var fairCases = []classCase{
	{"txhash", "GET", txHashQ, "", func(c *Classes) *class { return c.txhash }},
	{"bulk", "GET", "/block_results?height=5", "", func(c *Classes) *class { return c.bulk }},
	{"abci-query (lock)", "GET", abciSupply, "", func(c *Classes) *class { return c.abciQuery }},
	{"broadcast (lock)", "POST", "/", `{"jsonrpc":"2.0","id":7,"method":"broadcast_tx_sync","params":{"tx":"CgQKAggB"}}`, func(c *Classes) *class { return c.broadcast }},
	{"light", "GET", "/status", "", func(c *Classes) *class { return c.light }},
}

func (cc classCase) do(h http.Handler, ip string, auth string) int {
	var body io.Reader
	if cc.body != "" {
		body = strings.NewReader(cc.body)
	}
	r := httptest.NewRequest(cc.method, cc.target, body)
	if cc.body != "" {
		r.Header.Set("Content-Type", jsonCT)
	}
	if ip != "" {
		r.Header.Set(clientIPHeader, ip)
	}
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// R8-D-1: two clients hammering a class with as many requests as they like
// cannot starve a third. Each attacker keeps far more requests going than
// the class has slots (with a FIFO of requests rather than of clients, the
// third client's request would sit behind all of theirs and time out); the
// third client's requests are each served within the class's wait.
func TestTwoClientsCannotStarveAThird(t *testing.T) {
	const hold = 40 * time.Millisecond
	for _, cc := range fairCases {
		t.Run(cc.name, func(t *testing.T) {
			node := newSlowNode(t, hold)
			c := DefaultClasses()
			cl := cc.pick(c)
			cl.wait = 400 * time.Millisecond
			cl.timeout = 5 * time.Second
			rpc := NewRPC(node.srv.URL, client(), c)
			pub, _ := cl.caps()
			per := 8*pub + 8 // each attacker's goroutines
			stop := make(chan struct{})
			var wg sync.WaitGroup
			var attackerOK atomic.Int32
			for _, ip := range []string{"198.51.100.1", "2001:db8:bad::1"} {
				for i := 0; i < per; i++ {
					wg.Add(1)
					go func(ip string, i int) {
						defer wg.Done()
						if strings.Contains(ip, ":") {
							ip = fmt.Sprintf("2001:db8:bad::%x", i+1) // the same /64
						}
						for {
							select {
							case <-stop:
								return
							default:
							}
							if cc.do(rpc, ip, "") == http.StatusOK {
								attackerOK.Add(1)
							} else {
								time.Sleep(time.Millisecond)
							}
						}
					}(ip, i)
				}
			}
			time.Sleep(3 * hold) // the attackers have the class busy
			var worst time.Duration
			for i := 0; i < 8; i++ {
				start := time.Now()
				if code := cc.do(rpc, "203.0.113.7", ""); code != http.StatusOK {
					t.Errorf("third client's request %d: %d after %v", i, code, time.Since(start))
				}
				worst = max(worst, time.Since(start))
			}
			close(stop)
			wg.Wait()
			t.Logf("third client's slowest answer %v (hold %v), attackers served %d", worst, hold, attackerOK.Load())
			if attackerOK.Load() == 0 {
				t.Error("the attackers were never served: the test did not load the class")
			}
			if m := int(node.max.Load()); m > pub {
				t.Errorf("node worked on %d at once, %d public slots", m, pub)
			}
			deadline := time.Now().Add(2 * time.Second)
			for (used(cl) != 0 || cl.clientCount() != 0) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if n := cl.clientCount(); n != 0 {
				t.Errorf("%d client counters left after every request finished", n)
			}
		})
	}
}

// One client gets at most perClient requests to the node at once in a
// class, the rest 503 at once; another client is unaffected; the backend
// is exempt and keeps its reserve.
func TestPerClientLimit(t *testing.T) {
	g := newGate(t)
	c := DefaultClasses()
	c.txhash.timeout = 5 * time.Second
	auth := "Basic ZWFydGgtYmFja2VuZDp0b2tlbg=="
	sum := sha256.Sum256([]byte(auth))
	c.SetBackendAuthSHA256(sum[:])
	rpc := NewRPC(g.srv.URL, client(), c)
	cc := fairCases[0]
	var wg sync.WaitGroup
	codes := make(chan int, 64)
	run := func(n int, ip, auth string) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); codes <- cc.do(rpc, ip, auth) }()
		}
	}
	run(6, "198.51.100.1", "")
	waitFor(t, func() bool { return g.inFlight.Load() == 2 })
	start := time.Now()
	for i := 0; i < 4; i++ {
		if code := <-codes; code != http.StatusServiceUnavailable {
			t.Fatalf("over the per-client bound: %d", code)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatalf("over the per-client bound refused after %v, not at once", time.Since(start))
	}
	run(2, "198.51.100.2", "")
	waitFor(t, func() bool { return g.inFlight.Load() == 4 })
	// The backend, from the first client's address: past its bound, into the
	// public slots left (4) and its reserve (2).
	run(6, "198.51.100.1", auth)
	waitFor(t, func() bool { return g.inFlight.Load() == 10 })
	if pub, res := c.txhash.inUse(); pub != 8 || res != 2 {
		t.Fatalf("slots %d+%d, want 8+2", pub, res)
	}
	g.release()
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("a request within the bounds answered %d", code)
		}
	}
	waitFor(t, func() bool { return used(c.txhash) == 0 })
	if n := c.txhash.clientCount(); n != 0 {
		t.Fatalf("%d client counters left when idle", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

// The queue is served in arrival order; the backend takes its reserve past
// a full queue; a waiter that gives up leaves no count behind.
func TestClassQueue(t *testing.T) {
	cl := newClass("t", 1, 1, 1, time.Second, time.Second)
	ctx := t.Context()
	relA := cl.acquire(ctx, false, 1)
	if relA == nil {
		t.Fatal("first acquire")
	}
	if cl.acquire(ctx, false, 1) != nil {
		t.Fatal("second request of one client granted")
	}
	got := make(chan uint64, 3)
	for _, k := range []uint64{2, 3} {
		go func(k uint64) {
			if rel := cl.acquire(ctx, false, k); rel != nil {
				got <- k
				time.Sleep(20 * time.Millisecond)
				rel()
			}
		}(k)
		waitFor(t, func() bool { cl.mu.Lock(); defer cl.mu.Unlock(); return len(cl.queue) == int(k-1) })
	}
	relB := cl.acquire(ctx, true, 0) // the reserve
	if relB == nil {
		t.Fatal("backend reserve")
	}
	relA()
	if a, b := <-got, <-got; a != 2 || b != 3 {
		t.Fatalf("granted %d then %d, want 2 then 3", a, b)
	}
	relB()
	short := newClass("t", 1, 0, 1, 10*time.Millisecond, time.Second)
	rel := short.acquire(ctx, false, 1)
	if short.acquire(ctx, false, 2) != nil {
		t.Fatal("granted while full")
	}
	if short.clientCount() != 1 {
		t.Fatalf("a waiter that gave up left its count: %d clients", short.clientCount())
	}
	rel()
	if short.clientCount() != 0 {
		t.Fatal("counter kept after release")
	}
}
