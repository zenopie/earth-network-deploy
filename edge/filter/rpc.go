package filter

// The CometBFT RPC side (rpc.erth.network -> node:26657).
//
// What the node serves, and how this file reads each form
// (rpc/jsonrpc/server, v0.38.21):
//
//   - GET /<method>?a=..&b=..  (http_uri_handler.go): every route is also an
//     http.ServeMux pattern, served to ANY method; arguments come from
//     URL.Query().Get (first value), then r.FormValue (a POST form body).
//     Served here: GET only, no body, the route's arguments decoded as the
//     node does (rpcargs.go), unknown parameters dropped.
//   - POST / with a JSON-RPC body (http_json_handler.go): a single request
//     or a batch array; params a map or an array; a request without an id is
//     a notification and is not executed. Served here: a single request with
//     an id. Batches are refused (no client sends one, and one batch is many
//     calls under one slot). Any other path with a JSON-RPC body is the URI
//     form above, and its body is never read by the node, so refused.
//   - /websocket (ws_handler.go): every method over one connection. Refused.
//
// Each served call is checked (rpcpolicy.go) and forwarded as a request this
// proxy wrote, in the same form the client used: a URI GET with every
// argument in canonical encoding, or a JSON-RPC POST to / with canonical
// params. The node's answer is streamed back unchanged.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
)

type rpcArg struct {
	name string
	kind ArgKind
}

type rpcRoute struct {
	args []rpcArg
}

// rpcCall is one decoded call.
type rpcCall struct {
	method string
	args   map[string]ArgVal
}

type rpcHandler struct {
	up      *nodeClient
	classes *Classes
}

func (h *rpcHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isUpgrade(r) || r.URL.Path == "/websocket" {
		rpcRefuse(w, nil, http.StatusForbidden, "websockets are not served")
		return
	}
	if !cleanRawPath(r) {
		rpcRefuse(w, nil, http.StatusForbidden, "percent-encoded or malformed path")
		return
	}
	path := r.URL.Path
	if r.Method == http.MethodOptions {
		// A CORS preflight: no body, no arguments, answered by the node's
		// CORS handler for a path it serves.
		if path != "/" {
			if _, ok := rpcRoutes[strings.TrimPrefix(path, "/")]; !ok {
				rpcRefuse(w, nil, http.StatusForbidden, "method not served")
				return
			}
		}
		if !isPreflight(r) {
			// The node's CORS handler passes anything else through, and
			// a method path then runs its method, for any HTTP method.
			rpcRefuse(w, nil, http.StatusForbidden, "OPTIONS is served as a CORS preflight only")
			return
		}
		h.up.forward(w, r, h.classes.light, http.MethodOptions, path, nil, clientHeaders(r), fwdOpts{})
		return
	}
	if path == "/" {
		h.serveJSONRPC(w, r)
		return
	}
	h.serveURI(w, r)
}

func (h *rpcHandler) serveURI(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	route, ok := rpcRoutes[name]
	if !ok {
		rpcRefuse(w, nil, http.StatusForbidden, "method not served")
		return
	}
	if r.Method != http.MethodGet {
		rpcRefuse(w, nil, http.StatusMethodNotAllowed, "only GET on a method path (JSON-RPC goes to /)")
		return
	}
	if hasBody(r) {
		rpcRefuse(w, nil, http.StatusForbidden, "body on a GET")
		return
	}
	call, err := parseURICall(name, route, r.URL.RawQuery)
	if err != nil {
		rpcRefuse(w, nil, http.StatusBadRequest, err.Error())
		return
	}
	cl, err := checkRPC(h.classes, call)
	if err != nil {
		rpcRefuse(w, nil, http.StatusForbidden, err.Error())
		return
	}
	h.up.forward(w, r, cl, http.MethodGet, "/"+name+uriQuery(route, call), nil, clientHeaders(r), rpcFwdOpts(name))
}

// rpcFwdOpts: genesis_chunked is immutable for the chain's life (a relaunch
// purges Cloudflare's cache, RELAUNCH.md), ~1.75 MB a call: cacheable.
func rpcFwdOpts(method string) fwdOpts {
	if method == "genesis_chunked" {
		return fwdOpts{cache: "public, max-age=3600, s-maxage=86400"}
	}
	return fwdOpts{}
}

// parseURICall is httpParamsToArgs: for each of the route's arguments, the
// first value of that query parameter (URL.Query().Get), skipped when empty.
func parseURICall(name string, route rpcRoute, rawQuery string) (rpcCall, error) {
	q, _ := url.ParseQuery(rawQuery) // URL.Query() ignores the error too
	call := rpcCall{method: name, args: map[string]ArgVal{}}
	for _, a := range route.args {
		v := q.Get(a.name)
		if v == "" {
			continue
		}
		d, err := DecodeURIArg(a.kind, v)
		if err != nil {
			return call, fmt.Errorf("%s: %v", a.name, err)
		}
		if d.set {
			call.args[a.name] = d
		}
	}
	return call, nil
}

func uriQuery(route rpcRoute, call rpcCall) string {
	var parts []string
	for _, a := range route.args {
		if v := call.args[a.name]; v.set {
			parts = append(parts, a.name+"="+EncodeURI(a.kind, v))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "?" + strings.Join(parts, "&")
}

// jsonrpcRequest is types.RPCRequest's UnmarshalJSON shape. encoding/json
// matches field names case-insensitively and lets the last duplicate win,
// for the node and for this proxy alike.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

var errBatch = errors.New("batch requests are not served")

func (h *rpcHandler) serveJSONRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// GET / with no body is the node's HTML list of routes; with a body
		// it is JSON-RPC. Neither is needed.
		rpcRefuse(w, nil, http.StatusMethodNotAllowed, "JSON-RPC is POST to /")
		return
	}
	body, release, code := readBody(w, r, factorJSONRPC, h.classes.isBackend(r))
	if code != 0 {
		rpcRefuse(w, nil, code, http.StatusText(code))
		return
	}
	defer release()
	id, call, notification, err := parseJSONRPC(body)
	body = nil // the parsed call holds what is needed
	if err != nil {
		rpcRefuse(w, id, http.StatusBadRequest, err.Error())
		return
	}
	if notification {
		// The node executes nothing and answers nothing.
		w.WriteHeader(http.StatusOK)
		return
	}
	cl, err := checkRPC(h.classes, call)
	if err != nil {
		rpcRefuse(w, id, http.StatusForbidden, err.Error())
		return
	}
	hdr := clientHeaders(r)
	hdr.Set("Content-Type", "application/json")
	h.up.forward(w, r, cl, http.MethodPost, "/", jsonrpcBody(id, call), hdr, rpcFwdOpts(call.method))
}

// parseJSONRPC is makeJSONRPCHandler's decoding of one body. The id comes
// back canonical (a string, or an integer as the node truncates a float).
func parseJSONRPC(body []byte) (id interface{}, call rpcCall, notification bool, err error) {
	// A batch is an array: refused on its first byte, without decoding it
	// (decoding a 1 MiB array only to refuse it was a copy per element).
	if t := bytes.TrimLeft(body, " \t\r\n"); len(t) == 0 {
		return nil, call, false, errors.New("empty body")
	} else if t[0] == '[' {
		return nil, call, false, errBatch
	}
	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, call, false, fmt.Errorf("parse error: %v", err)
	}
	if req.ID == nil {
		return nil, call, true, nil
	}
	switch v := req.ID.(type) {
	case string:
		if len(v) > 128 {
			return nil, call, false, errors.New("id too long")
		}
		id = v
	case float64:
		if math.IsNaN(v) || math.Abs(v) > 1<<53 {
			return nil, call, false, errors.New("id out of range")
		}
		id = int64(v) // idFromInterface: int(id)
	default:
		return nil, call, false, fmt.Errorf("json-rpc ID (%v) is of unknown type", req.ID)
	}
	route, ok := rpcRoutes[req.Method]
	if !ok {
		return id, call, false, errors.New("method not served")
	}
	call = rpcCall{method: req.Method, args: map[string]ArgVal{}}
	if len(req.Params) == 0 {
		return id, call, false, nil
	}
	// jsonParamsToArgs: a map first, then an array.
	var m map[string]json.RawMessage
	if json.Unmarshal(req.Params, &m) == nil {
		for _, a := range route.args {
			p, ok := m[a.name]
			if !ok || p == nil || len(p) == 0 {
				continue
			}
			v, err := DecodeJSONArg(a.kind, p)
			if err != nil {
				return id, call, false, fmt.Errorf("%s: %v", a.name, err)
			}
			if v.set {
				call.args[a.name] = v
			}
		}
		return id, call, false, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(req.Params, &arr); err != nil {
		return id, call, false, errors.New("params must be a map or an array")
	}
	if len(arr) != len(route.args) {
		return id, call, false, fmt.Errorf("expected %d parameters, got %d", len(route.args), len(arr))
	}
	for i, a := range route.args {
		v, err := DecodeJSONArg(a.kind, arr[i])
		if err != nil {
			return id, call, false, fmt.Errorf("%s: %v", a.name, err)
		}
		if v.set {
			call.args[a.name] = v
		}
	}
	return id, call, false, nil
}

func jsonrpcBody(id interface{}, call rpcCall) []byte {
	params := map[string]json.RawMessage{}
	for _, a := range rpcRoutes[call.method].args {
		if v := call.args[a.name]; v.set {
			params[a.name] = EncodeJSON(a.kind, v)
		}
	}
	b, _ := json.Marshal(struct {
		JSONRPC string                     `json:"jsonrpc"`
		ID      interface{}                `json:"id"`
		Method  string                     `json:"method"`
		Params  map[string]json.RawMessage `json:"params"`
	}{"2.0", id, call.method, params})
	return b
}

// rpcRefuse answers in the node's own error shape, so CometBFT's client and
// the wallets print the reason. URI calls carry id -1, as the node's do.
func rpcRefuse(w http.ResponseWriter, id interface{}, code int, msg string) {
	if id == nil {
		id = -1
	}
	b, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]interface{}{
			"code":    -32600,
			"message": "refused by the edge filter",
			"data":    msg,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}
