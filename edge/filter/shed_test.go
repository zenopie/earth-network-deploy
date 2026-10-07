package filter

import (
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingBigNode answers n bytes to a path (and query) in big, and a
// short answer to anything else, counting what it was asked.
func countingBigNode(t *testing.T, n int, withLength bool, big ...string) (*httptest.Server, *atomic.Int32) {
	var asked atomic.Int32
	chunk := []byte(strings.Repeat("x", 64<<10))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		size := 16
		for _, b := range big {
			if r.URL.RequestURI() == b {
				size = n
			}
		}
		if withLength {
			w.Header().Set("Content-Length", strconv.Itoa(size))
		}
		for left := size; left > 0; {
			k := min(left, len(chunk))
			if _, err := w.Write(chunk[:k]); err != nil {
				return
			}
			left -= k
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func fetch(t *testing.T, url, auth string) (int, int64, error) {
	t.Helper()
	r, _ := http.NewRequest("GET", url, nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, n, err
}

// R8-D-1: once a tx's or a height's answer is found over its ceiling,
// public requests for it are refused without asking the node; the backend
// still gets it whole; other txs and heights are unaffected.
func TestOversizedAnswerShed(t *testing.T) {
	auth := "Basic ZWFydGgtYmFja2VuZDp0b2tlbg=="
	sum := sha256.Sum256([]byte(auth))
	const hash = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	cases := []struct {
		name, target, nodeURI, other string
		lcd                          bool
		ceiling                      int64
	}{
		{"rpc tx", "/tx?hash=0x" + hash, "/tx?hash=0x" + hash, "/tx?hash=0x" + strings.Repeat("ab", 32), false, maxRespTx},
		{"block_results", "/block_results?height=5", "/block_results?height=5", "/block_results?height=6", false, maxRespBlockResults},
		{"lcd tx", "/cosmos/tx/v1beta1/txs/" + hash, "/cosmos/tx/v1beta1/txs/" + hash, "/cosmos/tx/v1beta1/txs/" + strings.Repeat("ab", 32), true, maxRespTxLCD},
	}
	for _, tc := range cases {
		for _, withLength := range []bool{true, false} {
			t.Run(tc.name+map[bool]string{true: " declared", false: " streamed"}[withLength], func(t *testing.T) {
				node, asked := countingBigNode(t, int(tc.ceiling)+1, withLength, tc.nodeURI)
				c := DefaultClasses()
				c.SetBackendAuthSHA256(sum[:])
				var h http.Handler = NewRPC(node.URL, client(), c)
				if tc.lcd {
					h = NewLCD(node.URL, client(), c)
				}
				srv := httptest.NewServer(h)
				defer srv.Close()

				code, _, err := fetch(t, srv.URL+tc.target, "")
				if err == nil && code == http.StatusOK {
					t.Fatalf("oversized answer passed whole")
				}
				if asked.Load() != 1 {
					t.Fatalf("node asked %d times", asked.Load())
				}
				waitFor(t, func() bool { return c.answers.size() == 1 })
				for i := 0; i < 5; i++ {
					target := tc.target
					if tc.lcd && i%2 == 1 {
						target = strings.Replace(target, hash, strings.ToUpper(hash), 1) // the same tx
					}
					code, _, err := fetch(t, srv.URL+target, "")
					if err != nil || code != http.StatusBadGateway {
						t.Fatalf("shed request: %d %v", code, err)
					}
				}
				if asked.Load() != 1 {
					t.Fatalf("node asked again for a shed answer: %d", asked.Load())
				}
				// Another tx or height: asked.
				if code, _, err := fetch(t, srv.URL+tc.other, ""); err != nil || code != http.StatusOK {
					t.Fatalf("another key: %d %v", code, err)
				}
				// The backend: asked, the whole answer.
				code, n, err := fetch(t, srv.URL+tc.target, auth)
				if err != nil || code != http.StatusOK || n != tc.ceiling+1 {
					t.Fatalf("backend: %d, %d bytes, %v", code, n, err)
				}
				if asked.Load() != 3 {
					t.Fatalf("node asked %d times, want 3", asked.Load())
				}
				waitFor(t, func() bool { return used(c.txhash)+used(c.bulk) == 0 })
				if c.txhash.clientCount()+c.bulk.clientCount() != 0 {
					t.Fatal("client counters left")
				}
			})
		}
	}
}

// block_results of the latest block names no fixed answer: never shed.
func TestLatestBlockResultsNotShed(t *testing.T) {
	node, asked := countingBigNode(t, maxRespBlockResults+1, true, "/block_results")
	c := DefaultClasses()
	srv := httptest.NewServer(NewRPC(node.URL, client(), c))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		if code, _, _ := fetch(t, srv.URL+"/block_results", ""); code != http.StatusBadGateway {
			t.Fatalf("oversized latest block_results: %d", code)
		}
	}
	if asked.Load() != 2 || c.answers.size() != 0 {
		t.Fatalf("latest block_results shed: asked %d, %d keys", asked.Load(), c.answers.size())
	}
}

func TestShedListBounds(t *testing.T) {
	s := newShedList(50*time.Millisecond, 3)
	for _, k := range []string{"a", "b", "c", "d"} {
		s.add(k)
		time.Sleep(time.Millisecond)
	}
	if s.size() != 3 || s.has("a") || !s.has("d") || !s.has("b") {
		t.Fatalf("full list did not drop the oldest: %v", s.keys)
	}
	time.Sleep(60 * time.Millisecond)
	if s.has("d") {
		t.Fatal("expired key still shed")
	}
	if s.has("") {
		t.Fatal("empty key shed")
	}
}

// R8-D-1: a tx whose answer turned out heavy (a relay tx of megabytes, a
// loud contract call) is served to the public from the bulk class after its
// first build, so asking for it again and again cannot hold the txhash
// slots that commit polls need. The backend is not moved.
func TestHeavyAnswerMovesToBulk(t *testing.T) {
	auth := "Basic ZWFydGgtYmFja2VuZDp0b2tlbg=="
	sum := sha256.Sum256([]byte(auth))
	const hash = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	heavy := "/tx?hash=0x" + hash
	node, asked := countingBigNode(t, 2<<20, false, heavy)
	c := DefaultClasses()
	c.SetBackendAuthSHA256(sum[:])
	c.bulk.wait = 0
	srv := httptest.NewServer(NewRPC(node.URL, client(), c))
	defer srv.Close()

	if code, n, err := fetch(t, srv.URL+heavy, ""); err != nil || code != http.StatusOK || n != 2<<20 {
		t.Fatalf("heavy tx, first read: %d, %d bytes, %v", code, n, err)
	}
	waitFor(t, func() bool { return c.answers.get("rpc-tx/"+hash) == markHeavy })
	// Every public bulk slot busy (other clients' block_results).
	pub, _ := c.bulk.caps()
	var rels []func()
	for i := range pub {
		rels = append(rels, c.bulk.acquire(t.Context(), false, uint64(1000+i)))
	}
	if code, _, _ := fetch(t, srv.URL+heavy, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("heavy tx with bulk full: %d, want 503 (it must not use txhash)", code)
	}
	if code, _, _ := fetch(t, srv.URL+"/tx?hash=0x"+strings.Repeat("ab", 32), ""); code != http.StatusOK {
		t.Fatalf("an ordinary tx with bulk full: %d", code)
	}
	if code, n, err := fetch(t, srv.URL+heavy, auth); err != nil || code != http.StatusOK || n != 2<<20 {
		t.Fatalf("backend, heavy tx, bulk full: %d, %d bytes, %v", code, n, err)
	}
	for _, rel := range rels {
		rel()
	}
	before := asked.Load()
	if code, n, err := fetch(t, srv.URL+heavy, ""); err != nil || code != http.StatusOK || n != 2<<20 {
		t.Fatalf("heavy tx from bulk: %d, %d bytes, %v", code, n, err)
	}
	if asked.Load() != before+1 {
		t.Fatal("heavy tx not served")
	}
	waitFor(t, func() bool { return used(c.bulk)+used(c.txhash) == 0 })
}

func TestAnswerMarks(t *testing.T) {
	s := newShedList(time.Minute, 8)
	s.mark("k", markHeavy)
	if s.get("k") != markHeavy || s.has("k") {
		t.Fatal("heavy")
	}
	s.mark("k", markOver)
	s.mark("k", markHeavy) // never goes down while it lives
	if !s.has("k") {
		t.Fatal("over did not stick")
	}
}
