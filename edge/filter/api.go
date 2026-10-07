// Package filter is earth-edge's request filter: the CometBFT RPC and LCD
// allowlists, CometBFT's argument decoding, and the concurrency classes.
// See ../main.go for where it runs.
package filter

import (
	"net/http"
	"net/url"
	"strings"
)

// NewRPC serves rpc.erth.network, forwarding to the node's RPC at upstream
// (http://host:port).
func NewRPC(upstream string, client *http.Client, c *Classes) http.Handler {
	return marked(&rpcHandler{up: &nodeClient{base: upstream, client: client, classes: c}, classes: c})
}

// NewLCD serves lcd.erth.network, forwarding to the node's LCD at upstream.
func NewLCD(upstream string, client *http.Client, c *Classes) http.Handler {
	return marked(&lcdHandler{up: &nodeClient{base: upstream, client: client, classes: c}, classes: c, routes: lcdRoutes})
}

// EdgeHeader is on every answer this proxy gives, served or refused, and
// never on the node's own: an outside check (bin/check-edge.py) that finds it missing knows a public hostname points past
// the filter at the node (round-5 R5-E-8).
const EdgeHeader = "X-Earth-Edge"

func marked(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(EdgeHeader, "1")
		h.ServeHTTP(w, r)
	})
}

// Counts, for the start-up line.
func Counts() (lcdRouteCount, abciGRPCPaths int) {
	return len(lcdRoutes), len(abciGRPC)
}

// ABCIGRPCPaths lists the gRPC methods served over abci_query (for the
// conformance tests: none may take a PageRequest).
func ABCIGRPCPaths() []string {
	out := make([]string, 0, len(abciGRPC))
	for p := range abciGRPC {
		out = append(out, p)
	}
	return out
}

// Value exposes a decoded argument (for the conformance tests).
func (v ArgVal) Value() (set bool, s string, b []byte, i int64, t bool) {
	return v.set, v.s, v.b, v.i, v.t
}

// LCDRoute describes one served LCD route (for the conformance tests): its
// gRPC method ("" for the tx service) and whether pagination is served.
type LCDRoute struct {
	Method, Pattern, GRPC string
	Paginated             bool
}

// LCDRoutes lists the served LCD routes in match order.
func LCDRoutes() []LCDRoute {
	out := make([]LCDRoute, 0, len(lcdRoutes))
	for _, rt := range lcdRoutes {
		out = append(out, LCDRoute{rt.method, rt.pattern, rt.grpc, rt.page})
	}
	return out
}

// MatchLCDPath reports which served route a request with this method and
// raw path (no query) would be matched to, with the same path checks the
// handler makes.
func MatchLCDPath(method, rawPath string) (string, bool) {
	if strings.ContainsAny(rawPath, "?#") {
		return "", false
	}
	u, err := url.ParseRequestURI(rawPath) // as net/http reads a request target
	if err != nil {
		return "", false
	}
	parts, ok := splitLCDPath(&http.Request{Method: method, URL: u})
	if !ok {
		return "", false
	}
	h := &lcdHandler{routes: lcdRoutes}
	rt := h.find(method, parts)
	if rt == nil {
		return "", false
	}
	return rt.pattern, true
}

// AnswerCeilings lists the byte ceilings on public answers (forward.go), for
// the conformance test that checks them against the pinned chain's block and
// tx limits.
func AnswerCeilings() (tx, txLCD, blockResults, search, block, blockLCD int64) {
	return maxRespTx, maxRespTxLCD, maxRespBlockResults, maxRespSearch, maxRespBlock, maxRespBlockLCD
}

// ChainCaps lists the chain limits the ceilings are computed from
// (chaincaps.go), by the name the conformance test reads each under in the
// pinned chain.
func ChainCaps() map[string]int64 {
	out := make(map[string]int64, len(chainCaps))
	for k, v := range chainCaps {
		out[k] = v
	}
	return out
}

// TxAllowances: the stored-result bytes a tx may add outside the chain's
// msg-result cap (its ante events and log), and the JSON bytes per stored
// result byte, as the ceilings assume them (forward.go).
func TxAllowances() (anteBytes, jsonPerByte int64) {
	return txAnteBytes, jsonPerResultByte
}
