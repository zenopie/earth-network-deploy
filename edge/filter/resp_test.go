package filter

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bigNode answers every request with n bytes, chunked (no Content-Length)
// unless withLength.
func bigNode(t *testing.T, n int, withLength bool) *httptest.Server {
	chunk := []byte(strings.Repeat("x", 64<<10))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if withLength {
			w.Header().Set("Content-Length", strconv.Itoa(n))
		}
		for left := n; left > 0; {
			k := min(left, len(chunk))
			if _, err := w.Write(chunk[:k]); err != nil {
				return
			}
			left -= k
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const txHashQ = "/tx?hash=0x" + "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// R6-E-1: an answer past its ceiling never reaches a public client whole.
// Declared too large: 502 before anything is sent. Streamed past it: the
// connection is cut mid-body, so the client sees an error, not a short
// valid answer. The backend has no ceiling. The slot is freed either way.
func TestAnswerCeiling(t *testing.T) {
	auth := "Basic ZWFydGgtYmFja2VuZDp0b2tlbg=="
	sum := sha256.Sum256([]byte(auth))
	for _, withLength := range []bool{true, false} {
		node := bigNode(t, maxRespTx+1, withLength)
		c := DefaultClasses()
		c.SetBackendAuthSHA256(sum[:])
		srv := httptest.NewServer(NewRPC(node.URL, client(), c))
		defer srv.Close()

		resp, err := http.Get(srv.URL + txHashQ)
		if withLength {
			if err != nil || resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("declared oversize answer: %v %v", resp, err)
			}
			resp.Body.Close()
		} else {
			if err == nil {
				n, rerr := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if rerr == nil {
					t.Fatalf("streamed oversize answer read cleanly (%d bytes)", n)
				}
				if n > maxRespTx {
					t.Fatalf("passed %d bytes, ceiling %d", n, maxRespTx)
				}
			}
		}

		r, _ := http.NewRequest("GET", srv.URL+txHashQ, nil)
		r.Header.Set("Authorization", auth)
		resp, err = http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		n, rerr := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if rerr != nil || n != maxRespTx+1 || resp.StatusCode != http.StatusOK {
			t.Fatalf("backend read %d bytes (%v, %d), want the whole answer", n, rerr, resp.StatusCode)
		}

		deadline := time.Now().Add(2 * time.Second)
		for used(c.txhash) != 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if k := used(c.txhash); k != 0 {
			t.Fatalf("%d txhash slots still held", k)
		}
	}
	// Under the ceiling, an answer passes whole.
	node := bigNode(t, 1<<20, false)
	srv := httptest.NewServer(NewRPC(node.URL, client(), DefaultClasses()))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/block_results?height=5")
	if err != nil {
		t.Fatal(err)
	}
	n, rerr := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if rerr != nil || n != 1<<20 {
		t.Fatalf("1 MiB block_results: %d bytes, %v", n, rerr)
	}
}

// R6-E-1, R7-D-1: block_results is in the small bulk class; a tx by hash,
// RPC and LCD alike, is in its own txhash class, so a block_results flood
// cannot starve wallet commit polls. Each has a ceiling.
func TestChainSizedClasses(t *testing.T) {
	c := DefaultClasses()
	rpcCases := map[string]struct {
		call rpcCall
		want *class
	}{
		"block_results": {rpcCall{method: "block_results", args: map[string]ArgVal{}}, c.bulk},
		"tx":            {rpcCall{method: "tx", args: map[string]ArgVal{"hash": {set: true, b: make([]byte, 32)}}}, c.txhash},
	}
	for name, tc := range rpcCases {
		cl, err := checkRPC(c, tc.call)
		if err != nil || cl != tc.want {
			t.Errorf("%s: class %v, %v", name, cl, err)
		}
		if rpcFwdOpts(tc.call).maxResp == 0 {
			t.Errorf("%s: no answer ceiling", name)
		}
	}
	if rpcFwdOpts(rpcCall{method: "block"}).maxResp == 0 {
		t.Errorf("block: no answer ceiling")
	}
	for _, rt := range lcdRoutes {
		switch rt.pattern {
		case "/cosmos/tx/v1beta1/txs/{hash}":
			if rt.class(c) != c.txhash || rt.maxResp == 0 {
				t.Errorf("%s: class %s, ceiling %d", rt.pattern, rt.class(c).name, rt.maxResp)
			}
		case "/cosmos/tx/v1beta1/txs", "/cosmos/base/tendermint/v1beta1/blocks/{uint}", "/cosmos/base/tendermint/v1beta1/blocks/latest":
			if rt.method == "GET" && rt.maxResp == 0 {
				t.Errorf("%s: no answer ceiling", rt.pattern)
			}
		}
		if rt.class(c) == c.bulk {
			t.Errorf("%s: no LCD route shares block_results' class", rt.pattern)
		}
	}
	if n := slots(c.bulk); n > 4 {
		t.Errorf("bulk class holds %d", n)
	}
	if p, r := c.txhash.caps(); p < 8 || r < 1 {
		t.Errorf("txhash class %d+%d: too small for wallet commit polls", p, r)
	}
	// A tx by hash still gets an answer while every bulk slot is busy.
	p, _ := c.bulk.caps()
	for i := range p {
		if c.bulk.acquire(t.Context(), false, uint64(i)) == nil {
			t.Fatal("bulk slot refused")
		}
	}
	if rel := c.txhash.acquire(t.Context(), false, 99); rel == nil {
		t.Fatal("txhash slot refused while bulk is full")
	} else {
		rel()
	}
	// The ceilings stay under what the per-tx cap allows (forward.go).
	if maxRespTx < 8<<20 || maxRespTxLCD < 12<<20 {
		t.Errorf("tx ceilings %d/%d under the per-tx cap's worst case", maxRespTx, maxRespTxLCD)
	}
}

// R6-E-4: a client that stops reading a large answer does not keep its
// slot past writeStall.
func TestSlowReaderFreesSlot(t *testing.T) {
	oldStall := writeStall
	writeStall = 200 * time.Millisecond
	defer func() { writeStall = oldStall }()
	node := bigNode(t, 6<<20, false) // past any socket buffering, under the tx ceiling
	c := DefaultClasses()
	srv := httptest.NewServer(NewRPC(node.URL, client(), c))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4 << 10)
	}
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", txHashQ)
	// Never read. The slot is taken, then freed when a write stalls.
	deadline := time.Now().Add(time.Second)
	for used(c.txhash) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	for used(c.txhash) != 0 && time.Since(start) < 3*time.Second {
		time.Sleep(5 * time.Millisecond)
	}
	if used(c.txhash) != 0 {
		t.Fatalf("a client that stopped reading still holds its slot after %v", time.Since(start))
	}
}

// The slot is freed once the node's answer is read to its end, before the
// last write to the client.
func TestCopyAnswerReleasesAtEOF(t *testing.T) {
	var released bool
	w := &blockingWriter{ResponseRecorder: httptest.NewRecorder(), before: func() bool { return released }}
	_, ok := copyAnswer(w, strings.NewReader("short answer"), 0, func() { released = true })
	if !ok || !w.sawReleased {
		t.Fatalf("released before the last write: %v", w.sawReleased)
	}
}

type blockingWriter struct {
	*httptest.ResponseRecorder
	before      func() bool
	sawReleased bool
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.sawReleased = b.before()
	return b.ResponseRecorder.Write(p)
}

// Every slot of every class together fits in one upstream's connections,
// so a slot holder never queues in the transport behind another slot.
func TestSlotsFitConnections(t *testing.T) {
	n := 0
	for _, cl := range DefaultClasses().all() {
		n += slots(cl)
	}
	if max := NewTransport().MaxConnsPerHost; n > max {
		t.Fatalf("%d slots, %d connections per upstream", n, max)
	}
}
