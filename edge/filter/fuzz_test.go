package filter

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// recorder is an in-process "node": it records what the filter forwards.
type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var b []byte
	if req.Body != nil {
		b, _ = io.ReadAll(req.Body)
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.body = append(r.body, b)
	r.mu.Unlock()
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

func (r *recorder) take() ([]*http.Request, [][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q, b := r.reqs, r.body
	r.reqs, r.body = nil, nil
	return q, b
}

func fuzzRequest(method, target string, body []byte, ct string) (*http.Request, bool) {
	if !strings.HasPrefix(target, "/") || strings.ContainsAny(target, " \r\n\t#") {
		return nil, false
	}
	if _, err := url.ParseRequestURI(target); err != nil {
		return nil, false
	}
	switch method {
	case "GET", "POST", "OPTIONS", "PUT", "HEAD":
	default:
		return nil, false
	}
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	return r, true
}

// Whatever a client sends, what reaches the node is an allowed call in
// canonical form: re-reading it gives the same call, served by checkRPC.
func FuzzRPC(f *testing.F) {
	seeds := []struct{ m, t, b string }{
		{"GET", "/status", ""},
		{"GET", "/abci_query?path=%22/cosmos.bank.v1beta1.Query/SupplyOf%22&data=0x0a057565727468&height=1", ""},
		{"GET", "/abci_query?path=0x2f73746f72652f62616e6b2f7375627370616365", ""},
		{"GET", "/abci_query?path=%22/%5Cu0073tore/bank/subspace%22&prove=%74rue", ""},
		{"GET", "/block?height=%225%22", ""},
		{"GET", "/tx?hash=0x" + strings.Repeat("ab", 32) + "&prove=true", ""},
		{"POST", "/", `{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store/pki/key","data":"0A","height":"5","prove":true}}`},
		{"POST", "/", `{"jsonrpc":"2.0","id":"x","method":"validators","params":["5","1","100"]}`},
		{"POST", "/", `[{"jsonrpc":"2.0","id":1,"method":"status"}]`},
		{"POST", "/", `{"jsonrpc":"2.0","id":1,"METHOD":"tx_search","params":{"query":"tx.height>0"}}`},
		{"POST", "/", `{"jsonrpc":"2.0","id":6,"method":"abci_query","params":{"data":"0A00","path":"/cosmos.tx.v1beta1.Service/Simulate"}}`},
		{"POST", "/", `{"jsonrpc":"2.0","id":6,"method":"abci_query","params":{"data":"0A00","path":"/cosmos.bank.v1beta1.Query/AllBalances"}}`},
		{"GET", "/abci_info", ""},
		{"GET", "/genesis_chunked?chunk=0", ""},
		{"POST", "/", `{"jsonrpc":"2.0","id":7,"method":"broadcast_tx_sync","params":{"tx":"CgQKAggB"}}`},
	}
	for _, s := range seeds {
		f.Add(s.m, s.t, []byte(s.b))
	}
	rec := &recorder{}
	h := NewRPC("http://node", &http.Client{Transport: rec}, DefaultClasses())
	c := DefaultClasses()
	f.Fuzz(func(t *testing.T, method, target string, body []byte) {
		r, ok := fuzzRequest(method, target, body, "application/json")
		if !ok {
			return
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		reqs, bodies := rec.take()
		for i, fw := range reqs {
			switch fw.Method {
			case "GET":
				name := strings.TrimPrefix(fw.URL.Path, "/")
				route, ok := rpcRoutes[name]
				if !ok {
					t.Fatalf("forwarded GET %s", fw.URL.Path)
				}
				call, err := parseURICall(name, route, fw.URL.RawQuery)
				if err != nil {
					t.Fatalf("forwarded query %q does not parse: %v", fw.URL.RawQuery, err)
				}
				cl, err := checkRPC(c, call)
				if err != nil {
					t.Fatalf("forwarded %s?%s is refused on a second look: %v", name, fw.URL.RawQuery, err)
				}
				assertRPCCost(t, c, call, cl, 0)
				if q := uriQuery(route, call); q != "" && "?"+fw.URL.RawQuery != q || q == "" && fw.URL.RawQuery != "" {
					t.Fatalf("forwarded query %q is not canonical (%q)", fw.URL.RawQuery, q)
				}
			case "POST":
				if fw.URL.Path != "/" {
					t.Fatalf("forwarded POST %s", fw.URL.Path)
				}
				id, call, notif, err := parseJSONRPC(bodies[i])
				if err != nil || notif || id == nil {
					t.Fatalf("forwarded body %s does not parse: %v", bodies[i], err)
				}
				cl, err := checkRPC(c, call)
				if err != nil {
					t.Fatalf("forwarded %s is refused on a second look: %v", bodies[i], err)
				}
				assertRPCCost(t, c, call, cl, len(bodies[i]))
				if !bytes.Equal(jsonrpcBody(id, call), bodies[i]) {
					t.Fatalf("forwarded body %s is not canonical", bodies[i])
				}
			case "OPTIONS":
				if fw.ContentLength > 0 {
					t.Fatalf("preflight with a body")
				}
			default:
				t.Fatalf("forwarded %s", fw.Method)
			}
			for k := range fw.Header {
				switch k {
				case "Origin", "Accept", "Access-Control-Request-Method", "Access-Control-Request-Headers", "Content-Type":
				default:
					t.Fatalf("forwarded header %s", k)
				}
			}
		}
	})
}

func FuzzLCD(f *testing.F) {
	seeds := []struct{ m, t, b, ct string }{
		{"GET", "/cosmos/bank/v1beta1/balances/" + addr + "?pagination.limit=10", "", ""},
		{"GET", "/cosmos/tx/v1beta1/txs?query=tx.height%3D5&limit=20", "", ""},
		{"GET", "/cosmos/tx/v1beta1/txs?query=tx.height%3E0", "", ""},
		{"GET", "/cosmos/base/tendermint/v1beta1%2Fabci_query?path=/store/bank/subspace", "", ""},
		{"POST", "/cosmos/tx/v1beta1/txs", `{"tx_bytes":"AA==","mode":"BROADCAST_MODE_SYNC"}`, "application/json"},
		{"POST", "/cosmos/tx/v1beta1/txs", `query=tx.height%3E0`, "application/x-www-form-urlencoded"},
		{"GET", "/earth/personhood/v1/handles?start=a&limit=1000", "", ""},
		{"GET", "/cosmos/tx/v1beta1/txs?query=transfer.recipient%3D%27earth17xpfvakm2amg962yls6f84z3kell8c5lthcx95%27&limit=1", "", ""},
		{"GET", "/cosmos/tx/v1beta1/txs?query=tx.height%3D5", "", ""},
		{"GET", "/cosmos/staking/v1beta1/validators?pagination.count_total=true&pagination.limit=0", "", ""},
		{"GET", "/cosmos/gov/v1/proposals?pagination.offset=999999999", "", ""},
		{"GET", "/cosmos/bank/v1beta1/balances/" + addr, "", ""},
	}
	for _, s := range seeds {
		f.Add(s.m, s.t, []byte(s.b), s.ct)
	}
	rec := &recorder{}
	h := NewLCD("http://node", &http.Client{Transport: rec}, DefaultClasses())
	c := DefaultClasses()
	f.Fuzz(func(t *testing.T, method, target string, body []byte, ct string) {
		r, ok := fuzzRequest(method, target, body, ct)
		if !ok {
			return
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		reqs, bodies := rec.take()
		for i, fw := range reqs {
			m := fw.Method
			pat, ok := MatchLCDPath(m, fw.URL.Path)
			if m == "OPTIONS" {
				if m = "GET"; !ok {
					pat, ok = MatchLCDPath(m, fw.URL.Path)
				}
				if !ok {
					m = "POST"
					pat, ok = MatchLCDPath(m, fw.URL.Path)
				}
			}
			if !ok || fw.URL.RawPath != "" {
				t.Fatalf("forwarded %s %s", fw.Method, fw.URL.String())
			}
			var rt *lcdRoute
			for _, x := range lcdRoutes {
				if x.pattern == pat && x.method == m {
					rt = x
				}
			}
			q, err := url.ParseQuery(fw.URL.RawQuery)
			if err != nil {
				t.Fatalf("forwarded query %q", fw.URL.RawQuery)
			}
			for k, vs := range q {
				if rt.params[k] == nil || len(vs) != 1 || !rt.params[k].MatchString(vs[0]) {
					t.Fatalf("forwarded parameter %s=%v on %s", k, vs, pat)
				}
			}
			if rt.search && !searchRe.MatchString(q.Get("query")) {
				t.Fatalf("forwarded search %q", q.Get("query"))
			}
			if fw.Method == "GET" {
				assertLCDCost(t, rt, q)
			}
			if fw.Method == "POST" && rt.pattern == "/cosmos/tx/v1beta1/txs" && rt.class(c) != c.abci {
				t.Fatalf("a broadcast (CheckTx) outside the abci class")
			}
			if fw.Method == "POST" {
				if fw.Header.Get("Content-Type") != "application/json" || checkJSONObject(bodies[i], rt.body) != nil {
					t.Fatalf("forwarded POST body %q", bodies[i])
				}
			}
			for k := range fw.Header {
				switch k {
				case "Origin", "Accept", "Access-Control-Request-Method", "Access-Control-Request-Headers", "Content-Type", "X-Cosmos-Block-Height":
				default:
					t.Fatalf("forwarded header %s", k)
				}
			}
		}
	})
}

// Cost oracles (round-5 R5-E-9). The checks above prove that what is
// forwarded re-parses to an allowed call; these prove that every allowed
// call is a bounded one, from rules written here independently of the
// policy tables: a policy that admits an expensive call fails here even
// though it agrees with itself.

// boundedABCI: gRPC paths known to be point reads or keeper-capped pages.
// Adding a path to the policy means adding it here too, with the reason.
var boundedABCI = map[string]string{
	"/cosmos.auth.v1beta1.Query/Account":                 "point read",
	"/cosmos.auth.v1beta1.Query/ModuleAccountByName":     "point read",
	"/cosmos.gov.v1.Query/Proposal":                      "point read",
	"/earth.personhood.v1.Query/Registration":            "point read",
	"/earth.personhood.v1.Query/RegistrationsByDsc":      "one counter",
	"/earth.assembly.v1.Query/ProposalTally":             "stored tally",
	"/cosmos.bank.v1beta1.Query/SupplyOf":                "point read",
	"/earth.shielded.v1.Query/Tree":                      "tree size and root",
	"/earth.personhood.v1.Query/IdentityTree":            "tree size and root",
	"/earth.personhood.v1.Query/Handles":                 "keeper caps the page",
	"/earth.shieldedstaking.v1.Query/StakeTree":          "tree size and root",
	"/earth.shieldedstaking.v1.Query/StakeNullifierTree": "keeper caps the page (1000)",
	"/earth.shieldedstaking.v1.Query/DebtTree":           "keeper caps the page (1000)",
}

// mutexMethods take CometBFT's ABCI mutex.
var mutexMethods = map[string]bool{"abci_query": true, "broadcast_tx_sync": true, "broadcast_tx_async": true, "abci_info": true}

var boundedRPC = map[string]bool{
	"health": true, "status": true, "abci_info": true, "genesis_chunked": true, "block": true,
	"block_results": true, "commit": true, "consensus_params": true, "validators": true,
	"blockchain": true, "tx": true, "abci_query": true, "broadcast_tx_sync": true, "broadcast_tx_async": true,
}

func assertRPCCost(t *testing.T, c *Classes, call rpcCall, cl *class, bodyLen int) {
	t.Helper()
	if !boundedRPC[call.method] {
		t.Fatalf("forwarded %s, not a bounded method", call.method)
	}
	if mutexMethods[call.method] != (cl == c.abci) {
		t.Fatalf("%s in class %s: every ABCI-mutex call, and only those, is in the abci class", call.method, cl.name)
	}
	if bodyLen > 2*maxBody {
		t.Fatalf("forwarded a %d-byte body", bodyLen)
	}
	if call.method == "validators" {
		if v := call.args["per_page"]; v.set && v.i > 100 {
			t.Fatalf("validators per_page %d", v.i)
		}
	}
	if call.method != "abci_query" {
		return
	}
	path, data := call.args["path"].s, call.args["data"].b
	if strings.Contains(path, "Simulate") || strings.HasPrefix(path, "/cosmos.tx.") {
		t.Fatalf("abci_query %s: runs a tx or searches the index", path)
	}
	if strings.HasPrefix(path, "/store/") {
		parts := strings.Split(path, "/")
		if len(parts) != 4 || len(data) == 0 || len(data) > 512 {
			t.Fatalf("store read %s with %d-byte key", path, len(data))
		}
		if parts[3] == "subspace" {
			ok := (parts[2] == "pki" && (bytes.HasPrefix(data, []byte("csca_by_ski")) && len(data) >= 15 ||
				bytes.HasPrefix(data, []byte("csca_by_dn")) && len(data) >= 14)) ||
				(parts[2] == "personhood" && bytes.HasPrefix(data, []byte("regs_by_dsc")))
			if !ok {
				t.Fatalf("subspace read %s %x: not a known-small range", path, data)
			}
		} else if parts[3] != "key" {
			t.Fatalf("store read %s", path)
		}
		return
	}
	if _, ok := boundedABCI[path]; !ok {
		t.Fatalf("abci_query %s: not a known-bounded path", path)
	}
}

func assertLCDCost(t *testing.T, rt *lcdRoute, q url.Values) {
	t.Helper()
	if rt.pattern == "/cosmos/tx/v1beta1/txs" {
		query := q.Get("query")
		if !strings.HasPrefix(query, "tx.height=") || strings.ContainsAny(query[len("tx.height="):], " '<>!&|") {
			t.Fatalf("search %q: only one block's txs are bounded", query)
		}
		lim, err1 := strconv.Atoi(q.Get("limit"))
		page, err2 := strconv.Atoi(q.Get("page"))
		if err1 != nil || err2 != nil || lim < 1 || lim > 50 || page < 1 || page > 100 {
			t.Fatalf("search page limit=%q page=%q", q.Get("limit"), q.Get("page"))
		}
	}
	if !rt.page {
		for k := range q {
			if strings.HasPrefix(k, "pagination.") {
				t.Fatalf("pagination on an unpaginated route %s", rt.pattern)
			}
		}
		return
	}
	lim, err := strconv.Atoi(q.Get("pagination.limit"))
	if err != nil || lim < 1 || lim > 1000 {
		t.Fatalf("%s forwarded pagination.limit %q (absent or 0 turns on CountTotal)", rt.pattern, q.Get("pagination.limit"))
	}
	if q.Get("pagination.count_total") == "true" {
		t.Fatalf("%s forwarded count_total", rt.pattern)
	}
	if v := q.Get("pagination.offset"); v != "" {
		if off, err := strconv.Atoi(v); err != nil || off > 10000 {
			t.Fatalf("%s forwarded offset %q", rt.pattern, v)
		}
	}
}
