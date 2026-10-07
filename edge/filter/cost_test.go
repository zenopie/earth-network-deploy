package filter

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gate is a node that holds every request until released, counting how
// many it is working on at once.
type gate struct {
	srv          *httptest.Server
	open         chan struct{}
	inFlight     atomic.Int32
	maxInFlight  atomic.Int32
	seen         atomic.Int32
	releaseOnce  sync.Once
	answerStatus int
}

func newGate(t *testing.T) *gate {
	g := &gate{open: make(chan struct{}), answerStatus: http.StatusOK}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		g.seen.Add(1)
		n := g.inFlight.Add(1)
		for {
			m := g.maxInFlight.Load()
			if n <= m || g.maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		<-g.open // the node works on it regardless of the client
		g.inFlight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.answerStatus)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(func() { g.release(); g.srv.Close() })
	return g
}

func (g *gate) release() { g.releaseOnce.Do(func() { close(g.open) }) }

func fastClasses() *Classes {
	c := DefaultClasses()
	for _, cl := range []*class{c.light, c.results, c.query, c.abci, c.simulate, c.search} {
		cl.wait = 20 * time.Millisecond
		cl.timeout = 100 * time.Millisecond
	}
	return c
}

func client() *http.Client { return &http.Client{Transport: NewTransport()} }

const abciSupply = `/abci_query?path=%22/cosmos.bank.v1beta1.Query/SupplyOf%22&data=0x0a057565727468`

// R5-E-1, R5-E-3: a request whose client deadline passes keeps its slot
// until the node answers it. The node never has more requests of a class
// than the class's cap, however many clients time out and retry.
func TestSlotHeldUntilNodeAnswers(t *testing.T) {
	g := newGate(t)
	c := fastClasses()
	rpc := NewRPC(g.srv.URL, client(), c)
	codes := map[int]int{}
	for i := 0; i < 10; i++ {
		w := do(rpc, get(abciSupply))
		codes[w.Code]++
	}
	// The first two (the public abci cap) time out at the client with 504;
	// the node is still working on them, so every later one is refused.
	if codes[http.StatusGatewayTimeout] != 2 || codes[http.StatusServiceUnavailable] != 8 {
		t.Fatalf("codes %v, want 2x504 and 8x503", codes)
	}
	if n := g.maxInFlight.Load(); n != 2 {
		t.Fatalf("node worked on %d abci requests at once, cap is 2", n)
	}
	g.release()
	// Once the node answers, the slots are free again.
	deadline := time.Now().Add(2 * time.Second)
	for len(c.abci.sem) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if w := do(rpc, get(abciSupply)); w.Code != http.StatusOK {
		t.Fatalf("after the node answered: %d", w.Code)
	}
}

// R5-E-3: every door into the ABCI mutex shares one class: RPC abci_query,
// RPC and LCD broadcasts, abci_info.
func TestABCIClassShared(t *testing.T) {
	g := newGate(t)
	c := fastClasses()
	c.abci.timeout = 5 * time.Second
	rpc := NewRPC(g.srv.URL, client(), c)
	lcd := NewLCD(g.srv.URL, client(), c)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); do(rpc, get(abciSupply)) }()
	}
	for g.inFlight.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	others := map[string]struct {
		h http.Handler
		q req
	}{
		"rpc store read":    {rpc, post(`{"jsonrpc":"2.0","id":2,"method":"abci_query","params":{"data":"0A0B","path":"/store/pki/key"}}`)},
		"rpc broadcast":     {rpc, post(`{"jsonrpc":"2.0","id":7,"method":"broadcast_tx_sync","params":{"tx":"CgQKAggB"}}`)},
		"rpc abci_info":     {rpc, get("/abci_info")},
		"lcd broadcast":     {lcd, req{method: "POST", target: "/cosmos/tx/v1beta1/txs", body: `{"tx_bytes":"CgQKAggB","mode":"BROADCAST_MODE_SYNC"}`, header: map[string]string{"Content-Type": jsonCT}}},
		"rpc abci gRPC GET": {rpc, get("/abci_query?path=%22/earth.shielded.v1.Query/Tree%22")},
	}
	for name, o := range others {
		if w := do(o.h, o.q); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s while the abci class is full: %d, want 503", name, w.Code)
		}
	}
	// LCD queries run outside the mutex: their own class.
	if w := do(lcd, get("/cosmos/bank/v1beta1/params")); w.Code == http.StatusServiceUnavailable {
		t.Errorf("an LCD query waited on the abci class")
	}
	g.release()
	wg.Wait()
}

// R5-E-7: the backend's credential reaches slots the public cannot fill.
func TestBackendReserve(t *testing.T) {
	g := newGate(t)
	c := fastClasses()
	c.abci.timeout = 5 * time.Second
	auth := "Basic ZWFydGgtYmFja2VuZDp0b2tlbg=="
	sum := sha256.Sum256([]byte(auth))
	c.SetBackendAuthSHA256(sum[:])
	rpc := NewRPC(g.srv.URL, client(), c)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); do(rpc, get(abciSupply)) }()
	}
	for g.inFlight.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	if w := do(rpc, req{method: "GET", target: abciSupply, header: map[string]string{"Authorization": "Basic d3Jvbmc="}}); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a wrong credential got a slot: %d", w.Code)
	}
	wg.Add(1)
	var backendCode int
	go func() {
		defer wg.Done()
		backendCode = do(rpc, req{method: "GET", target: abciSupply, header: map[string]string{"Authorization": auth}}).Code
	}()
	for g.inFlight.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	g.release()
	wg.Wait()
	if backendCode != http.StatusOK {
		t.Fatalf("backend request with the public slots full: %d", backendCode)
	}
	if got := g.maxInFlight.Load(); got != 3 {
		t.Fatalf("node saw %d at once, want 2 public + 1 reserved", got)
	}
}

// R5-E-8: every answer of the edge carries X-Earth-Edge, refusals included.
func TestEdgeHeader(t *testing.T) {
	rpc, lcd, _ := handlers(t)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"rpc served":  do(rpc, get("/status")),
		"rpc refused": do(rpc, get("/tx_search?query=%22tx.height%3E0%22")),
		"lcd served":  do(lcd, get("/cosmos/bank/v1beta1/params")),
		"lcd refused": do(lcd, get("/cosmos/tx/v1beta1/txs?query=message.sender%3D%27"+addr+"%27")),
	} {
		if w.Header().Get(EdgeHeader) != "1" {
			t.Errorf("%s: no %s", name, EdgeHeader)
		}
	}
}

// R5-E-9: genesis_chunked is immutable for the chain's life: cacheable.
func TestGenesisChunkedCacheable(t *testing.T) {
	rpc, _, _ := handlers(t)
	w := do(rpc, get("/genesis_chunked?chunk=0"))
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "public") || !strings.Contains(cc, "s-maxage=") {
		t.Fatalf("genesis_chunked Cache-Control %q", cc)
	}
	if cc := do(rpc, get("/status")).Header().Get("Cache-Control"); strings.Contains(cc, "public") {
		t.Fatalf("status made cacheable: %q", cc)
	}
}

// R5-E-6: a client that dribbles its body holds no class slot and is cut
// off at its body's deadline, while other requests are served.
func TestSlowBodyShed(t *testing.T) {
	node := newFakeNode(t)
	c := DefaultClasses()
	srv := httptest.NewServer(NewRPC(node.srv.URL, client(), c))
	defer srv.Close()

	const declared = 4096 // deadline readDeadline(4096) = 2 s + 31 ms
	var slow []net.Conn
	for i := 0; i < 40; i++ {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n{", declared)
		slow = append(slow, conn)
	}
	time.Sleep(100 * time.Millisecond)
	// No slot of any class is held by the 40 dribblers.
	for _, cl := range []*class{c.light, c.results, c.query, c.abci, c.simulate, c.search} {
		if len(cl.sem) != 0 {
			t.Fatalf("a slow body holds a %s slot", cl.name)
		}
	}
	start := time.Now()
	resp, err := http.Post(srv.URL+"/", jsonCT, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"broadcast_tx_sync","params":{"tx":"CgQKAggB"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || time.Since(start) > time.Second {
		t.Fatalf("a broadcast behind 40 slow bodies: %d after %v", resp.StatusCode, time.Since(start))
	}
	// Each slow body is answered (408) or dropped by its deadline.
	for _, conn := range slow {
		_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err == nil && !strings.Contains(line, "408") && !strings.Contains(line, "400") {
			t.Fatalf("slow body answered %q", line)
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("slow body not cut off at its deadline")
		}
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("slow bodies cut off after %v", time.Since(start))
	}
	if publicBodies.inUse() != 0 {
		t.Fatalf("budget not returned: %d", publicBodies.inUse())
	}
}

// R5-E-5: a burst of 1 MiB broadcasts against a node that answers slowly
// cannot take the heap past the budget: excess bodies are refused 503, the
// budget is never overdrawn, and the peak heap stays far below the edge's
// 192 MiB limit (round 5 measured 214 MiB RSS for this burst).
func TestBurstMemoryBound(t *testing.T) {
	if testing.Short() {
		t.Skip("burst test")
	}
	debug.SetMemoryLimit(160 << 20) // the SDL's GOMEMLIMIT
	defer debug.SetMemoryLimit(-1)
	g := newGate(t)
	c := DefaultClasses()
	srv := httptest.NewServer(NewRPC(g.srv.URL, client(), c))
	defer srv.Close()

	tx := bytes.Repeat([]byte("A"), maxBody-200) // base64 of ~750 KiB, a ~1 MiB body
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"broadcast_tx_sync","params":{"tx":"` + string(tx) + `"}}`)
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var peak atomic.Uint64
	var over atomic.Bool
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
			if publicBodies.inUse() > bodyBudget {
				over.Store(true)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	tr := &http.Transport{MaxConnsPerHost: 300, MaxIdleConnsPerHost: 300, DisableCompression: true}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 250; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := hc.Post(srv.URL+"/", jsonCT, bytes.NewReader(body))
			code := -1
			if err == nil {
				code = resp.StatusCode
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			mu.Lock()
			codes[code]++
			mu.Unlock()
		}()
	}
	time.Sleep(3 * time.Second) // the node holds what reached it
	g.release()
	wg.Wait()
	close(stop)
	<-sampled
	t.Logf("codes %v, peak heap in use %d MiB over a %d MiB base", codes, peak.Load()>>20, base.HeapInuse>>20)
	if over.Load() {
		t.Fatalf("body budget overdrawn")
	}
	if codes[http.StatusServiceUnavailable] == 0 {
		t.Fatalf("no request refused: the burst did not reach the budget (%v)", codes)
	}
	// The test process holds the client side too; the budget is 48 MiB.
	if grew := peak.Load() - base.HeapInuse; grew > 110<<20 {
		t.Fatalf("heap grew %d MiB under the burst", grew>>20)
	}
	if publicBodies.inUse() != 0 {
		t.Fatalf("budget not returned: %d", publicBodies.inUse())
	}
}

// A body is never read past maxBody, chunked or not.
func TestBodyCaps(t *testing.T) {
	node := newFakeNode(t)
	srv := httptest.NewServer(NewLCD(node.srv.URL, client(), DefaultClasses()))
	defer srv.Close()
	big := strings.NewReader(`{"tx_bytes":"` + strings.Repeat("A", maxBody) + `"}`)
	r, _ := http.NewRequest("POST", srv.URL+"/cosmos/tx/v1beta1/txs", io.NopCloser(big)) // no length: chunked
	r.Header.Set("Content-Type", jsonCT)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversize body: %d", resp.StatusCode)
	}
	if len(node.take()) != 0 || publicBodies.inUse() != 0 {
		t.Fatalf("oversize body forwarded or budget kept")
	}
}

func TestBudgetBigShare(t *testing.T) {
	b := newBudget(100)
	if !b.take(70, 70) || b.take(10, 10) {
		t.Fatalf("big share is 3/4")
	}
	if !b.take(30, 0) || b.take(1, 0) {
		t.Fatalf("small bodies use the rest, then nothing")
	}
	b.give(70, 70)
	b.give(30, 0)
	if b.inUse() != 0 {
		t.Fatal("not returned")
	}
}
