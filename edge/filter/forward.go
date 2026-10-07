package filter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Response headers worth passing back. Everything else the node sets
// (X-Server-Time, its Date) is dropped; Go adds its own framing.
var passResponseHeaders = []string{
	"Content-Type",
	"Cache-Control",
	"Vary",
	"Access-Control-Allow-Origin",
	"Access-Control-Allow-Credentials",
	"Access-Control-Allow-Methods",
	"Access-Control-Allow-Headers",
	"Access-Control-Expose-Headers",
	"Access-Control-Max-Age",
	// The LCD reports the height a query answered at.
	"Grpc-Metadata-X-Cosmos-Block-Height",
}

type nodeClient struct {
	base    string // http://node:26657, no trailing slash
	client  *http.Client
	classes *Classes
}

func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy:               nil,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		MaxConnsPerHost:     64,
		IdleConnTimeout:     60 * time.Second,
		// No ResponseHeaderTimeout: forward's context (holdCeiling) bounds a
		// request, and a shorter transport timeout would free a class slot
		// while the node still works on the request.
		DisableCompression: true, // Cloudflare compresses towards clients
		ForceAttemptHTTP2:  false,
	}
}

// fwdOpts: per-call extras for forward.
type fwdOpts struct {
	// cache, if set, is the Cache-Control sent with a 200 (genesis_chunked:
	// immutable for the chain's life, so Cloudflare can serve it).
	cache string
	// maxResp, if set, is the most answer bytes passed to a public client
	// (the backend has none: its indexer must read every height). An answer
	// that declares more is refused 502 before anything is sent; one that
	// streams past it is cut off mid-body (the connection is aborted, so
	// the client sees a broken answer, never a short valid one).
	maxResp int64
}

// Answer ceilings (round-6 R6-E-1), for the calls whose answer size is set
// by chain data. A backstop only: by the time the edge counts bytes the node
// has built the whole answer, so the ceiling stops the copy (the edge's and
// Cloudflare's bandwidth, and the node's write of the rest), not the node's
// memory. The chain's per-tx result byte cap is what bounds that build;
// these are set well above any answer that cap and the block limits allow
// (block max_bytes 22 MiB, mempool max_tx_bytes 1 MiB, base64 in JSON).
const (
	maxRespTx           = 8 << 20  // RPC tx, LCD txs/{hash}: one tx and its result
	maxRespBlockResults = 32 << 20 // one block's results
	maxRespSearch       = 32 << 20 // LCD tx.height=N: at most 50 txs with results
	maxRespBlock        = 48 << 20 // RPC block: a full block, base64 in JSON
	maxRespBlockLCD     = 96 << 20 // LCD block: block and sdk_block, both in full
)

// Downstream writes (round-6 R6-E-4). The slot is held while the answer
// streams to the client, because the node is still writing it (it builds
// the whole answer first, then writes; until the edge has read it the
// node holds it in memory). A client that reads slowly would therefore
// keep the node's answer and the slot alive. Each write to the client must
// finish within writeStall, and the whole copy within writeMax; past either
// the connection is dropped and the slot freed. Once the node's answer has
// been read to its end, the slot is released before the last write.
var (
	writeStall = 10 * time.Second
	writeMax   = 45 * time.Second
)

// upstreamResult is what the node answered.
type upstreamResult struct {
	resp *http.Response
	err  error
}

// forward sends a request the proxy built to the node under one slot of cl,
// and streams the answer back.
//
// The slot is held until the node has answered, not until the client's
// deadline: the node keeps working on a request whose client has gone (it
// neither notices the dropped connection nor gives up its place in the ABCI
// mutex queue), so releasing early would admit more work than the cap says
// (round-5 R5-E-1, R5-E-3). When the client's deadline (cl.timeout) passes
// first, the client gets 504 and a goroutine keeps the slot until the node
// answers, then closes that answer unread. At most cap such goroutines exist
// per class. holdCeiling bounds even that: a node silent that long is
// wedged, and the request is dropped.
//
// After the node answers, the slot is held while its answer streams to the
// client, until the answer has been read from the node to its end (see
// writeStall, writeMax): at most writeMax longer, less for a client that
// reads at all.
func (u *nodeClient) forward(w http.ResponseWriter, r *http.Request, cl *class,
	method, pathQuery string, body []byte, header http.Header, o fwdOpts) {
	backend := u.classes.isBackend(r)
	acquired := cl.acquire(r.Context(), backend)
	if acquired == nil {
		busy(w, r)
		return
	}
	var once sync.Once
	release := func() { once.Do(acquired) }
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), holdCeiling)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.base+pathQuery, rd)
	if err != nil {
		cancel()
		release()
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	done := make(chan upstreamResult, 1)
	go func() {
		resp, err := u.client.Do(req)
		done <- upstreamResult{resp, err}
	}()
	deadline := time.NewTimer(cl.timeout)
	defer deadline.Stop()
	var res upstreamResult
	select {
	case res = <-done:
	case <-deadline.C:
		go abandon(done, cancel, release)
		http.Error(w, http.StatusText(http.StatusGatewayTimeout), http.StatusGatewayTimeout)
		return
	case <-r.Context().Done():
		go abandon(done, cancel, release)
		return
	}
	defer release()
	defer cancel()
	if res.err != nil {
		code := http.StatusBadGateway
		if errors.Is(res.err, context.DeadlineExceeded) {
			code = http.StatusGatewayTimeout
		}
		http.Error(w, http.StatusText(code), code)
		return
	}
	defer res.resp.Body.Close()
	limit := o.maxResp
	if backend {
		limit = 0
	}
	if limit > 0 && res.resp.ContentLength > limit {
		http.Error(w, "answer too large", http.StatusBadGateway)
		return
	}
	h := w.Header()
	for _, k := range passResponseHeaders {
		if vs := res.resp.Header.Values(k); len(vs) > 0 {
			h[http.CanonicalHeaderKey(k)] = vs
		}
	}
	if o.cache != "" && res.resp.StatusCode == http.StatusOK {
		h.Set("Cache-Control", o.cache)
	}
	w.WriteHeader(res.resp.StatusCode)
	if !copyAnswer(w, res.resp.Body, limit, release) {
		// Past the ceiling: the status is already sent, so the only honest
		// answer is a broken one. The deferred closes and release run.
		panic(http.ErrAbortHandler)
	}
}

// copyAnswer streams the node's answer to the client: at most limit bytes
// (0: no limit), each write under writeStall and all of them under writeMax.
// It calls release as soon as the node's answer has been read to its end.
// It returns false only when the answer passed limit.
func copyAnswer(w http.ResponseWriter, src io.Reader, limit int64, release func()) bool {
	rc := http.NewResponseController(w)
	end := time.Now().Add(writeMax)
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, err := io.ReadFull(src, buf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			release() // the node is done with this request
		}
		total += int64(n)
		if limit > 0 && total > limit {
			return false
		}
		if n > 0 {
			d := time.Now().Add(writeStall)
			if d.After(end) {
				d = end
			}
			// ErrNotSupported only off a real connection (tests).
			_ = rc.SetWriteDeadline(d)
			if _, werr := w.Write(buf[:n]); werr != nil {
				return true
			}
		}
		if err != nil {
			return true
		}
	}
}

// abandon waits, holding the slot, until the node answers a request whose
// client has gone, then drops the answer and frees the slot.
func abandon(done <-chan upstreamResult, cancel context.CancelFunc, release func()) {
	res := <-done
	if res.resp != nil {
		res.resp.Body.Close()
	}
	cancel()
	release()
}

// clientHeaders copies the few request headers the node may see: CORS
// (the node answers preflights and sets Allow-Origin itself) and Accept.
// Nothing that names the client (CF-Connecting-IP, X-Forwarded-For, Cookie,
// Authorization, User-Agent) and nothing that changes how the node routes
// or decodes (X-HTTP-Method-Override, Content-Type from the client,
// Grpc-Metadata-*) goes through.
func clientHeaders(r *http.Request, extra ...string) http.Header {
	h := http.Header{}
	for _, k := range append([]string{"Origin", "Accept",
		"Access-Control-Request-Method", "Access-Control-Request-Headers"}, extra...) {
		if v := r.Header.Get(k); v != "" && len(v) <= 512 && printableASCII(v) {
			h.Set(k, v)
		}
	}
	return h
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// hasBody: whether the client sent (or announced) a request body.
func hasBody(r *http.Request) bool {
	return r.ContentLength != 0 || len(r.TransferEncoding) > 0
}

// isUpgrade: a websocket (or any other protocol switch). Never served: the
// node's /websocket carries every RPC method over one connection.
func isUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" || r.Header.Get("Sec-WebSocket-Key") != "" {
		return true
	}
	for _, v := range r.Header.Values("Connection") {
		if strings.Contains(strings.ToLower(v), "upgrade") {
			return true
		}
	}
	return false
}

// cleanRawPath: the path exactly as sent has no percent-escapes, so the
// decoded path a server routes on is byte-for-byte what was checked here.
func cleanRawPath(r *http.Request) bool {
	if r.URL.RawPath != "" || strings.ContainsRune(r.URL.EscapedPath(), '%') {
		return false
	}
	// Go's server accepts an absolute-form request target; the path is still
	// r.URL.Path. Opaque and authority forms have no path to route.
	return r.URL.Opaque == "" && strings.HasPrefix(r.URL.Path, "/")
}

// isPreflight: a CORS preflight, which the node's CORS handler answers
// without running anything. No body, no query.
func isPreflight(r *http.Request) bool {
	return r.Header.Get("Origin") != "" && r.Header.Get("Access-Control-Request-Method") != "" &&
		!hasBody(r) && r.URL.RawQuery == ""
}

func busy(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "busy", http.StatusServiceUnavailable)
}
