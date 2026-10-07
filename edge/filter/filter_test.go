package filter

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeNode records every request that reaches "the node".
type fakeNode struct {
	mu   sync.Mutex
	reqs []seen
	srv  *httptest.Server
}

type seen struct {
	method, path, rawQuery string
	body                   []byte
	header                 http.Header
}

func newFakeNode(t *testing.T) *fakeNode {
	n := &fakeNode{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		n.mu.Lock()
		n.reqs = append(n.reqs, seen{r.Method, r.URL.Path, r.URL.RawQuery, b, r.Header.Clone()})
		n.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Server-Time", "1")
		w.Header().Set("Grpc-Metadata-X-Cosmos-Block-Height", "7")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":-1,"result":{}}`))
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeNode) take() []seen {
	n.mu.Lock()
	defer n.mu.Unlock()
	r := n.reqs
	n.reqs = nil
	return r
}

type req struct {
	method, target string
	body           string
	header         map[string]string
}

func do(h http.Handler, q req) *httptest.ResponseRecorder {
	var body io.Reader
	if q.body != "" {
		body = strings.NewReader(q.body)
	}
	r := httptest.NewRequest(q.method, q.target, body)
	for k, v := range q.header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func handlers(t *testing.T) (rpc, lcd http.Handler, node *fakeNode) {
	node = newFakeNode(t)
	c := &http.Client{Transport: NewTransport()}
	return NewRPC(node.srv.URL, c, DefaultClasses()), NewLCD(node.srv.URL, c, DefaultClasses()), node
}

func hx(s string) string  { return "0x" + hex.EncodeToString([]byte(s)) }
func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

const (
	addr   = "earth1qyqszqgpqyqszqgpqyqszqgpqyqszqgp5f3k9z"
	valop  = "earthvaloper1qyqszqgpqyqszqgpqyqszqgpqyqszqgpz7t6sg"
	txHash = "5B1C2D3E4F5061728394A5B6C7D8E9F00112233445566778899AABBCCDDEEFF0"
	jsonCT = "application/json"
)

var hash32 = bytes.Repeat([]byte{0xab}, 32)

func post(body string) req {
	return req{method: "POST", target: "/", body: body, header: map[string]string{"Content-Type": jsonCT}}
}

func get(target string) req { return req{method: "GET", target: target} }

// Every bypass named in the round-3 and round-4 edge reports (R3-BD-1,
// R3-BD-2, R4-E-1, R4-E-2), and the obvious neighbours of each. Refused,
// and nothing reaches the node.
func TestRPCRefused(t *testing.T) {
	rpc, _, node := handlers(t)
	getTxsEvent := "/cosmos.tx.v1beta1.Service/GetTxsEvent"
	supply := "%22/cosmos.bank.v1beta1.Query/SupplyOf%22"
	cases := map[string]req{
		// index scans, every transport
		"tx_search GET":           get("/tx_search?query=%22tx.height%3E0%22"),
		"tx_search POST /":        post(`{"jsonrpc":"2.0","id":1,"method":"tx_search","params":{"query":"tx.height>0"}}`),
		"tx_search POST path":     {method: "POST", target: "/tx_search", body: `query="tx.height>0"`, header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
		"block_search GET":        get("/block_search?query=%22block.height%3E0%22"),
		"block_search POST":       post(`{"jsonrpc":"2.0","id":1,"method":"block_search","params":{"query":"block.height>0"}}`),
		"tx_search METHOD key":    post(`{"jsonrpc":"2.0","id":1,"METHOD":"tx_search","params":{"query":"tx.height>0"}}`),
		"tx_search duplicate key": post(`{"jsonrpc":"2.0","id":1,"method":"status","Method":"tx_search","params":{"query":"tx.height>0"}}`),
		"tx_search array params":  post(`{"jsonrpc":"2.0","id":1,"method":"tx_search","params":["tx.height>0",false,"1","100","desc"]}`),
		// websockets
		"websocket upgrade":        {method: "GET", target: "/websocket", header: map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}},
		"websocket plain":          get("/websocket"),
		"upgrade on a method path": {method: "GET", target: "/status", header: map[string]string{"Connection": "upgrade", "Upgrade": "websocket"}},
		"subscribe POST":           post(`{"jsonrpc":"2.0","id":1,"method":"subscribe","params":{"query":"tm.event='Tx'"}}`),
		// batches
		"batch of status":   post(`[{"jsonrpc":"2.0","id":1,"method":"status"}]`),
		"batch hiding scan": post(`[{"jsonrpc":"2.0","id":1,"method":"status"},{"jsonrpc":"2.0","id":2,"method":"tx_search","params":{"query":"tx.height>0"}}]`),
		"batch whitespace":  post(" \n\t[{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"status\"}]"),
		"empty batch":       post(`[]`),
		// abci_query: CometBFT's own second decoding (R4-E-1)
		"abci hex GetTxsEvent":       get("/abci_query?path=" + hx(getTxsEvent) + "&data=0x0a00&x=/cosmos.bank.v1beta1.Query/SupplyOf"),
		"abci HEX upper GetTxsEvent": get("/abci_query?path=0X" + strings.TrimPrefix(hx(getTxsEvent), "0x")),
		"abci json escape Service":   get("/abci_query?path=%22/cosmos.tx.v1beta1.Ser%5Cu0076ice/GetTxsEvent%22&x=/cosmos.bank.v1beta1.Query/SupplyOf"),
		"abci json escape store":     get("/abci_query?path=%22/%5Cu0073tore/bank/subspace%22&x=/cosmos.bank.v1beta1.Query/SupplyOf"),
		"abci hex store subspace":    get("/abci_query?path=0x2f73746f72652f62616e6b2f7375627370616365"),
		"abci pct-encoded Service":   get("/abci_query?path=%22/cosmos%2Etx%2Ev1beta1%2EService/GetTxsEvent%22"),
		"abci decoy then real":       get("/abci_query?path=%22" + getTxsEvent + "%22&path=" + supply),
		"abci prove %74rue":          get("/abci_query?path=" + supply + "&data=0x0a057565727468&prove=%74rue"),
		"abci prove space true":      get("/abci_query?path=" + supply + "&data=0x0a057565727468&prove=%20true"),
		"abci prove JSON-RPC":        post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.bank.v1beta1.Query/SupplyOf","data":"0A057565727468","prove":true}}`),
		"abci json GetTxsEvent":      post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.tx.v1beta1.Service/GetTxsEvent"}}`),
		"abci json escape in JSON":   post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store/bank/subspace","data":"00"}}`),
		"abci GetTx over abci":       post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.tx.v1beta1.Service/GetTx"}}`),
		"abci BroadcastTx":           post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.tx.v1beta1.Service/BroadcastTx"}}`),
		"abci reflection":            post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.base.reflection.v2alpha1.ReflectionService/GetQueryServicesDescriptor"}}`),
		"abci cmt GetBlockWithTxs":   post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.tx.v1beta1.Service/GetBlockWithTxs"}}`),
		// R5-E-2: Simulate runs the whole tx inside the ABCI mutex.
		"abci Simulate JSON-RPC": post(`{"jsonrpc":"2.0","id":6,"method":"abci_query","params":{"data":"0A00","height":"0","path":"/cosmos.tx.v1beta1.Service/Simulate","prove":false}}`),
		"abci Simulate GET":      get("/abci_query?path=%22/cosmos.tx.v1beta1.Service/Simulate%22&data=0x00"),
		// R5-E-3: no paginated method over abci_query (no LCD mirror).
		"abci AllBalances":              post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.bank.v1beta1.Query/AllBalances","data":"0A00"}}`),
		"abci Validators":               post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.staking.v1beta1.Query/Validators","data":"1A0218FF"}}`),
		"abci Burns":                    post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/earth.earth.v1.Query/Burns"}}`),
		"abci SigningInfos":             get("/abci_query?path=%22/cosmos.slashing.v1beta1.Query/SigningInfos%22&data=0x0a0420012001"),
		"abci /app/simulate":            post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/app/simulate","data":"00"}}`),
		"abci /p2p":                     post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/p2p/filter/id/abc"}}`),
		"abci /custom":                  post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/custom/bank/x"}}`),
		"abci no leading slash":         post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"store/pki/key","data":"00"}}`),
		"abci store trailing slash":     post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store/pki/key/","data":"00"}}`),
		"abci store double slash":       post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store//pki/key","data":"00"}}`),
		"abci store bank key":           post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store/bank/key","data":"00"}}`),
		"abci store empty key":          post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store/pki/key"}}`),
		"abci subspace bank":            post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store/bank/subspace","data":"00"}}`),
		"abci subspace registrations":   post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store/personhood/subspace","data":"` + strings.ToUpper(hex.EncodeToString([]byte("registrations"))) + `"}}`),
		"abci subspace bare collection": post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store/pki/subspace","data":"` + strings.ToUpper(hex.EncodeToString([]byte("csca_by_ski"))) + `"}}`),
		"abci subspace one byte":        post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store/shielded/subspace","data":"06"}}`),
		"abci subspace prove":           post(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/store/personhood/subspace","data":"` + strings.ToUpper(hex.EncodeToString([]byte("regs_by_dsc"))) + `","prove":true}}`),
		"abci subspace via hex GET":     get("/abci_query?path=" + hx("/store/personhood/subspace") + "&data=" + hx("reg_by_dsc")),
		// method paths take GET only, and no body
		"POST form to method path": {method: "POST", target: "/abci_query", body: "path=%22/store/bank/subspace%22", header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
		"POST JSON to method path": {method: "POST", target: "/status", body: `{"jsonrpc":"2.0","id":1,"method":"tx_search"}`, header: map[string]string{"Content-Type": jsonCT}},
		"GET with a body":          {method: "GET", target: "/abci_query", body: "path=x"},
		"HEAD":                     {method: "HEAD", target: "/status"},
		"PUT":                      {method: "PUT", target: "/status"},
		"GET / (route list)":       get("/"),
		"GET / with JSON body":     {method: "GET", target: "/", body: `{"jsonrpc":"2.0","id":1,"method":"status"}`},
		// paths
		"trailing slash":        get("/status/"),
		"double slash":          get("//status"),
		"upper case":            get("/STATUS"),
		"pct letter":            get("/st%61tus"),
		"pct slash":             get("/status%2F"),
		"pct slash method":      get("/tx%5Fsearch?query=x"),
		"dot segment":           get("/./status"),
		"OPTIONS runs a method": {method: "OPTIONS", target: "/abci_query?path=%22/store/bank/subspace%22"},
		"OPTIONS with query":    {method: "OPTIONS", target: "/abci_query?path=x", header: map[string]string{"Origin": "https://erth.network", "Access-Control-Request-Method": "GET"}},
		"unknown":               get("/net_info"),
		// other routes not served
		"broadcast_tx_commit": post(`{"jsonrpc":"2.0","id":1,"method":"broadcast_tx_commit","params":{"tx":"AA=="}}`),
		"check_tx":            post(`{"jsonrpc":"2.0","id":1,"method":"check_tx","params":{"tx":"AA=="}}`),
		"genesis":             get("/genesis"),
		"dump_consensus":      get("/dump_consensus_state"),
		"unconfirmed":         get("/unconfirmed_txs?limit=%22100%22"),
		"dial_seeds":          post(`{"jsonrpc":"2.0","id":1,"method":"dial_seeds","params":{"seeds":["x"]}}`),
		"broadcast_evidence":  post(`{"jsonrpc":"2.0","id":1,"method":"broadcast_evidence","params":{}}`),
		"unsafe_flush":        get("/unsafe_flush_mempool"),
		// argument checks
		"tx short hash":       get("/tx?hash=0xabcd"),
		"validators per_page": post(`{"jsonrpc":"2.0","id":1,"method":"validators","params":{"per_page":"1000"}}`),
		"bad int encoding":    post(`{"jsonrpc":"2.0","id":1,"method":"block","params":{"height":5}}`),
		"lone quote int":      get(`/block?height=%22`),
		"hex where int":       get("/block?height=0x10"),
		"empty tx":            post(`{"jsonrpc":"2.0","id":1,"method":"broadcast_tx_sync","params":{"tx":""}}`),
		"bad id type":         post(`{"jsonrpc":"2.0","id":true,"method":"status"}`),
		"params wrong length": post(`{"jsonrpc":"2.0","id":1,"method":"block","params":["1","2"]}`),
		"not JSON":            post(`status`),
		"oversized body":      post(`{"jsonrpc":"2.0","id":1,"method":"broadcast_tx_sync","params":{"tx":"` + strings.Repeat("A", maxBody) + `"}}`),
	}
	for name, q := range cases {
		w := do(rpc, q)
		if w.Code == http.StatusOK {
			t.Errorf("%s: served (%d) %s", name, w.Code, w.Body.String())
		}
		if got := node.take(); len(got) != 0 {
			t.Errorf("%s: reached the node: %+v", name, got[0])
		}
	}
}

// Every call the clients make (the inventory in the deploy README), each
// served, and what reaches the node is the canonical form.
func TestRPCServed(t *testing.T) {
	rpc, _, node := handlers(t)
	regsByDsc := strings.ToUpper(hex.EncodeToString([]byte("regs_by_dsc")))
	ski := strings.ToUpper(hex.EncodeToString(append([]byte("csca_by_ski"), bytes.Repeat([]byte{0x14}, 20)...)))
	dn := strings.ToUpper(hex.EncodeToString(append([]byte("csca_by_dn"), bytes.Repeat([]byte{0x20}, 32)...)))
	type want struct{ method, path, query string }
	cases := []struct {
		name string
		q    req
		want want
	}{
		// wallets, web app, backend indexer: GET
		{"status", get("/status"), want{"GET", "/status", ""}},
		{"blockchain", get("/blockchain?minHeight=1&maxHeight=20"), want{"GET", "/blockchain", "minHeight=1&maxHeight=20"}},
		{"genesis_chunked", get("/genesis_chunked?chunk=0"), want{"GET", "/genesis_chunked", ""}},
		{"genesis_chunked 1", get("/genesis_chunked?chunk=1"), want{"GET", "/genesis_chunked", "chunk=1"}},
		{"block", get("/block?height=5"), want{"GET", "/block", "height=5"}},
		{"block latest (docs curl)", get("/block"), want{"GET", "/block", ""}},
		{"block_results", get("/block_results?height=5"), want{"GET", "/block_results", "height=5"}},
		{"web SupplyOf", get("/abci_query?path=%22/cosmos.bank.v1beta1.Query/SupplyOf%22&data=0x0a057565727468&height=1"),
			want{"GET", "/abci_query", "path=" + hx("/cosmos.bank.v1beta1.Query/SupplyOf") + "&data=0x0a057565727468&height=1"}},
		{"backend Tree (httpx)", get("/abci_query?path=%22%2Fearth.shielded.v1.Query%2FTree%22&data=0x&height=100"),
			want{"GET", "/abci_query", "path=" + hx("/earth.shielded.v1.Query/Tree") + "&data=0x&height=100"}},
		{"backend StakeNullifierTree", get("/abci_query?path=%22%2Fearth.shieldedstaking.v1.Query%2FStakeNullifierTree%22&data=0x1001&height=9"),
			want{"GET", "/abci_query", "path=" + hx("/earth.shieldedstaking.v1.Query/StakeNullifierTree") + "&data=0x1001&height=9"}},
		{"backend Handles", get("/abci_query?path=%22%2Fearth.personhood.v1.Query%2FHandles%22&data=0x10e807&height=9"),
			want{"GET", "/abci_query", "path=" + hx("/earth.personhood.v1.Query/Handles") + "&data=0x10e807&height=9"}},
		{"backend known DSCs", get("/abci_query?path=%22%2Fstore%2Fpersonhood%2Fsubspace%22&data=0x726567735f62795f647363"),
			want{"GET", "/abci_query", "path=" + hx("/store/personhood/subspace") + "&data=0x726567735f62795f647363"}},
		{"decoy parameter dropped", get("/abci_query?path=%22/cosmos.bank.v1beta1.Query/SupplyOf%22&x=%2Fstore%2Fbank%2Fsubspace"),
			want{"GET", "/abci_query", "path=" + hx("/cosmos.bank.v1beta1.Query/SupplyOf")}},
		{"first of repeated wins", get("/abci_query?path=%22/cosmos.bank.v1beta1.Query/SupplyOf%22&path=%22/store/bank/subspace%22"),
			want{"GET", "/abci_query", "path=" + hx("/cosmos.bank.v1beta1.Query/SupplyOf")}},
		// earthd --node, gas-check, state sync: JSON-RPC POST (rpchttp's encoding)
		{"rpchttp status", post(`{"jsonrpc":"2.0","id":0,"method":"status","params":{}}`), want{"POST", "/", ""}},
		{"rpchttp block", post(`{"jsonrpc":"2.0","id":1,"method":"block","params":{"height":"5"}}`), want{"POST", "/", ""}},
		{"gas-check key", post(`{"jsonrpc":"2.0","id":2,"method":"abci_query","params":{"data":"0A0B","height":"5","path":"/store/pki/key","prove":false}}`), want{"POST", "/", ""}},
		{"gas-check key prove", post(`{"jsonrpc":"2.0","id":3,"method":"abci_query","params":{"data":"0A0B","height":"5","path":"/store/personhood/key","prove":true}}`), want{"POST", "/", ""}},
		{"gas-check shielded key", post(`{"jsonrpc":"2.0","id":3,"method":"abci_query","params":{"data":"00","height":"5","path":"/store/shielded/key","prove":false}}`), want{"POST", "/", ""}},
		{"gas-check csca_by_ski", post(`{"jsonrpc":"2.0","id":4,"method":"abci_query","params":{"data":"` + ski + `","height":"5","path":"/store/pki/subspace","prove":false}}`), want{"POST", "/", ""}},
		{"gas-check csca_by_dn", post(`{"jsonrpc":"2.0","id":4,"method":"abci_query","params":{"data":"` + dn + `","height":"5","path":"/store/pki/subspace","prove":false}}`), want{"POST", "/", ""}},
		{"known DSCs JSON", post(`{"jsonrpc":"2.0","id":4,"method":"abci_query","params":{"data":"` + regsByDsc + `","path":"/store/personhood/subspace"}}`), want{"POST", "/", ""}},
		{"cli Account", post(`{"jsonrpc":"2.0","id":5,"method":"abci_query","params":{"data":"0A00","height":"0","path":"/cosmos.auth.v1beta1.Query/Account","prove":false}}`), want{"POST", "/", ""}},
		{"cli module account", post(`{"jsonrpc":"2.0","id":6,"method":"abci_query","params":{"data":"0A03676F76","height":"0","path":"/cosmos.auth.v1beta1.Query/ModuleAccountByName","prove":false}}`), want{"POST", "/", ""}},
		{"runbook registrations-by-dsc", post(`{"jsonrpc":"2.0","id":6,"method":"abci_query","params":{"data":"0A00","height":"0","path":"/earth.personhood.v1.Query/RegistrationsByDsc","prove":false}}`), want{"POST", "/", ""}},
		{"cli gov proposal", post(`{"jsonrpc":"2.0","id":6,"method":"abci_query","params":{"data":"0801","height":"0","path":"/cosmos.gov.v1.Query/Proposal","prove":false}}`), want{"POST", "/", ""}},
		{"cli assembly tally", post(`{"jsonrpc":"2.0","id":6,"method":"abci_query","params":{"data":"0801","height":"0","path":"/earth.assembly.v1.Query/ProposalTally","prove":false}}`), want{"POST", "/", ""}},
		{"cli broadcast", post(`{"jsonrpc":"2.0","id":7,"method":"broadcast_tx_sync","params":{"tx":"CgQKAggB"}}`), want{"POST", "/", ""}},
		{"cli query tx", post(`{"jsonrpc":"2.0","id":8,"method":"tx","params":{"hash":"` + b64(hash32) + `","prove":true}}`), want{"POST", "/", ""}},
		{"light commit", post(`{"jsonrpc":"2.0","id":9,"method":"commit","params":{"height":"100"}}`), want{"POST", "/", ""}},
		{"light commit latest", post(`{"jsonrpc":"2.0","id":9,"method":"commit","params":{}}`), want{"POST", "/", ""}},
		{"light validators", post(`{"jsonrpc":"2.0","id":10,"method":"validators","params":{"height":"100","page":"1","per_page":"100"}}`), want{"POST", "/", ""}},
		{"statesync consensus_params", post(`{"jsonrpc":"2.0","id":11,"method":"consensus_params","params":{"height":"100"}}`), want{"POST", "/", ""}},
		{"string id", post(`{"jsonrpc":"2.0","id":"abc","method":"status"}`), want{"POST", "/", ""}},
		{"array params", post(`{"jsonrpc":"2.0","id":1,"method":"block","params":["5"]}`), want{"POST", "/", ""}},
		{"null params", post(`{"jsonrpc":"2.0","id":1,"method":"status","params":null}`), want{"POST", "/", ""}},
		// preflights
		{"rpc preflight", req{method: "OPTIONS", target: "/abci_query", header: map[string]string{"Origin": "https://erth.network", "Access-Control-Request-Method": "GET"}}, want{"OPTIONS", "/abci_query", ""}},
	}
	for _, c := range cases {
		w := do(rpc, c.q)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
			continue
		}
		got := node.take()
		if len(got) != 1 {
			t.Errorf("%s: node saw %d requests", c.name, len(got))
			continue
		}
		g := got[0]
		if g.method != c.want.method || g.path != c.want.path || (c.want.method == "GET" && g.rawQuery != c.want.query) {
			t.Errorf("%s: node got %s %s?%s, want %s %s?%s", c.name, g.method, g.path, g.rawQuery, c.want.method, c.want.path, c.want.query)
		}
		if g.method == "POST" {
			// The forwarded body decodes to the same call.
			id, call, notif, err := parseJSONRPC(g.body)
			_, orig, _, _ := parseJSONRPC([]byte(c.q.body))
			if err != nil || notif || id == nil || !sameCall(call, orig) {
				t.Errorf("%s: forwarded body %s does not decode to the same call (%v)", c.name, g.body, err)
			}
		}
		if w.Header().Get("X-Server-Time") != "" {
			t.Errorf("%s: X-Server-Time passed back", c.name)
		}
	}
}

func sameCall(a, b rpcCall) bool {
	if a.method != b.method || len(a.args) != len(b.args) {
		return false
	}
	for k, v := range a.args {
		w := b.args[k]
		if v.set != w.set || v.s != w.s || !bytes.Equal(v.b, w.b) || v.i != w.i || v.t != w.t {
			return false
		}
	}
	return true
}

func TestRPCNotification(t *testing.T) {
	rpc, _, node := handlers(t)
	w := do(rpc, post(`{"jsonrpc":"2.0","method":"tx_search","params":{"query":"tx.height>0"}}`))
	if w.Code != http.StatusOK || w.Body.Len() != 0 || len(node.take()) != 0 {
		t.Fatalf("a notification is answered with nothing and never forwarded: %d %q", w.Code, w.Body.String())
	}
}

func TestRPCHeadersStripped(t *testing.T) {
	rpc, _, node := handlers(t)
	do(rpc, req{method: "GET", target: "/status", header: map[string]string{
		"CF-Connecting-IP": "203.0.113.9", "X-Forwarded-For": "203.0.113.9", "User-Agent": "x",
		"Authorization": "Basic Zm9vOmJhcg==", "Cookie": "a=b", "Origin": "https://erth.network",
	}})
	g := node.take()[0]
	for _, k := range []string{"Cf-Connecting-Ip", "X-Forwarded-For", "Authorization", "Cookie"} {
		if g.header.Get(k) != "" {
			t.Errorf("%s reached the node", k)
		}
	}
	if g.header.Get("Origin") != "https://erth.network" {
		t.Errorf("Origin not forwarded (CORS)")
	}
	if ua := g.header.Get("User-Agent"); strings.Contains(ua, "x") && ua == "x" {
		t.Errorf("client User-Agent reached the node")
	}
}

func TestLCDRefused(t *testing.T) {
	_, lcd, node := handlers(t)
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "X-HTTP-Method-Override": "GET"}
	cases := map[string]req{
		// R4-E-2: an encoded slash is a slash to the gateway
		"pct-slash abci_query": get("/cosmos/base/tendermint/v1beta1%2Fabci_query?path=/store/bank/subspace&data="),
		"pct-slash tx search":  get("/cosmos/tx%2Fv1beta1/txs?query=tx.height%3E0"),
		"pct-encoded letter":   get("/cosmos/bank/v1beta1/bal%61nces/" + addr),
		"lower pct-slash":      get("/cosmos/tx/v1beta1%2ftxs?query=tx.height%3E0"),
		"abci_query route":     get("/cosmos/base/tendermint/v1beta1/abci_query?path=%22/store/bank/subspace%22"),
		"abci_query prove":     get("/cosmos/base/tendermint/v1beta1/abci_query?path=/store/bank/key&data=AA%3D%3D&prove=true"),
		// R3-BD-1 (LCD half): the form POST method override
		"form POST override":     {method: "POST", target: "/cosmos/tx/v1beta1/txs", body: "query=tx.height%3E0", header: form},
		"form POST no override":  {method: "POST", target: "/cosmos/tx/v1beta1/txs", body: "query=tx.height%3E0", header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
		"form POST to GET route": {method: "POST", target: "/cosmos/bank/v1beta1/balances/" + addr, body: "x=1", header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
		"override header on GET": {method: "GET", target: "/cosmos/bank/v1beta1/params", header: map[string]string{"X-HTTP-Method-Override": "POST"}},
		"override lower case":    {method: "POST", target: "/cosmos/tx/v1beta1/txs", body: `{"tx_bytes":"AA=="}`, header: map[string]string{"Content-Type": jsonCT, "x-http-method-override": "GET"}},
		"grpc-web to a service":  {method: "POST", target: "/cosmos.tx.v1beta1.Service/GetTxsEvent", body: "\x00\x00\x00\x00\x00", header: map[string]string{"Content-Type": "application/grpc-web+proto"}},
		"grpc-web on broadcast":  {method: "POST", target: "/cosmos/tx/v1beta1/txs", body: "\x00", header: map[string]string{"Content-Type": "application/grpc-web"}},
		"text/plain broadcast":   {method: "POST", target: "/cosmos/tx/v1beta1/txs", body: `{"tx_bytes":"AA=="}`, header: map[string]string{"Content-Type": "text/plain"}},
		// R3-BD-2: the search, however it is spelled
		"search range":         get("/cosmos/tx/v1beta1/txs?query=tx.height%3E0&order_by=ORDER_BY_DESC&limit=20"),
		"search range %71uery": get("/cosmos/tx/v1beta1/txs?%71uery=tx.height%3E0"),
		"search events":        get("/cosmos/tx/v1beta1/txs?events=tx.height%3E0"),
		"search AND":           get("/cosmos/tx/v1beta1/txs?query=message.sender%3D%27" + addr + "%27%20AND%20tx.height%3E0"),
		"search contains":      get("/cosmos/tx/v1beta1/txs?query=message.sender%20CONTAINS%20%27e%27"),
		"search big limit":     get("/cosmos/tx/v1beta1/txs?query=tx.height%3D5&limit=100"),
		// R5-E-1: an address equality is a whole-history scan (the fee
		// collector receives every fee), whatever the limit.
		"search fee collector":         get("/cosmos/tx/v1beta1/txs?query=transfer.recipient%3D%27earth17xpfvakm2amg962yls6f84z3kell8c5lthcx95%27&limit=1"),
		"search sender":                get("/cosmos/tx/v1beta1/txs?query=message.sender%3D%27" + addr + "%27&order_by=ORDER_BY_DESC&limit=20"),
		"search recipient":             get("/cosmos/tx/v1beta1/txs?query=transfer.recipient%3D%27" + addr + "%27"),
		"search sender and height":     get("/cosmos/tx/v1beta1/txs?query=message.sender%3D%27" + addr + "%27%20AND%20tx.height%3D5"),
		"search hash event":            get("/cosmos/tx/v1beta1/txs?query=tx.hash%3D%27" + txHash + "%27"),
		"search height int64 overflow": get("/cosmos/tx/v1beta1/txs?query=tx.height%3D99999999999999999999"),
		"search repeated query":        get("/cosmos/tx/v1beta1/txs?query=tx.height%3D5&query=tx.height%3E0"),
		"search POST JSON":             {method: "POST", target: "/cosmos/tx/v1beta1/txs", body: `{"query":"tx.height>0"}`, header: map[string]string{"Content-Type": jsonCT}},
		"search over grpc-gw verb":     get("/cosmos/tx/v1beta1/txs:search?query=tx.height%3E0"),
		// path shapes
		"trailing slash": get("/cosmos/tx/v1beta1/txs/"),
		"double slash":   get("/cosmos//tx/v1beta1/txs/" + txHash),
		"dot segment":    get("/cosmos/tx/./v1beta1/txs/" + txHash),
		"dotdot segment": get("/cosmos/bank/v1beta1/../../tx/v1beta1/txs?query=tx.height%3E0"),
		"upper case":     get("/Cosmos/bank/v1beta1/params"),
		"verb":           get("/cosmos/bank/v1beta1/params:x"),
		"empty var":      get("/cosmos/auth/v1beta1/accounts/"),
		"bad hash":       get("/cosmos/tx/v1beta1/txs/block"),
		"block with txs": get("/cosmos/tx/v1beta1/txs/block/5"),
		"not an address": get("/cosmos/bank/v1beta1/balances/x"),
		"swagger":        get("/swagger/"),
		"metrics":        get("/metrics"),
		"root":           get("/"),
		"unlisted route": get("/earth/shieldedstaking/v1/moves/abc"),
		// methods and bodies
		"PUT":                    {method: "PUT", target: "/cosmos/tx/v1beta1/txs", body: "{}", header: map[string]string{"Content-Type": jsonCT}},
		"DELETE":                 {method: "DELETE", target: "/cosmos/bank/v1beta1/params"},
		"HEAD":                   {method: "HEAD", target: "/cosmos/bank/v1beta1/params"},
		"POST to a GET route":    {method: "POST", target: "/cosmos/bank/v1beta1/params", body: "{}", header: map[string]string{"Content-Type": jsonCT}},
		"GET with body":          {method: "GET", target: "/cosmos/bank/v1beta1/params", body: "x"},
		"broadcast extra field":  {method: "POST", target: "/cosmos/tx/v1beta1/txs", body: `{"tx_bytes":"AA==","query":"x"}`, header: map[string]string{"Content-Type": jsonCT}},
		"broadcast array":        {method: "POST", target: "/cosmos/tx/v1beta1/txs", body: `[{"tx_bytes":"AA=="}]`, header: map[string]string{"Content-Type": jsonCT}},
		"broadcast trailing":     {method: "POST", target: "/cosmos/tx/v1beta1/txs", body: `{"tx_bytes":"AA=="}{"x":1}`, header: map[string]string{"Content-Type": jsonCT}},
		"broadcast dup key":      {method: "POST", target: "/cosmos/tx/v1beta1/txs", body: `{"tx_bytes":"AA==","tx_bytes":"AQ=="}`, header: map[string]string{"Content-Type": jsonCT}},
		"broadcast query string": {method: "POST", target: "/cosmos/tx/v1beta1/txs?mode=x", body: `{"tx_bytes":"AA=="}`, header: map[string]string{"Content-Type": jsonCT}},
		"websocket":              {method: "GET", target: "/cosmos/bank/v1beta1/params", header: map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}},
		// parameters
		"page limit too big": get("/cosmos/bank/v1beta1/balances/" + addr + "?pagination.limit=100000"),
		// R5-E-4: CountTotal, a zero limit (which turns CountTotal on), and
		// long skips are walks of the collection.
		"page count_total":   get("/cosmos/staking/v1beta1/validators?pagination.count_total=true"),
		"page limit zero":    get("/cosmos/staking/v1beta1/validators?pagination.limit=0"),
		"page limit 000":     get("/cosmos/staking/v1beta1/validators?pagination.limit=000"),
		"page big offset":    get("/cosmos/gov/v1/proposals?pagination.offset=999999999&pagination.limit=1"),
		"page offset 10001":  get("/cosmos/gov/v1/proposals?pagination.offset=10001"),
		"repeated param":     get("/earth/personhood/v1/handles?limit=10&limit=20"),
		"bad height header":  {method: "GET", target: "/earth/shielded/v1/tree", header: map[string]string{"X-Cosmos-Block-Height": "1; drop"}},
		"semicolon query":    get("/cosmos/bank/v1beta1/params?a=1;b=2"),
		"preflight unlisted": {method: "OPTIONS", target: "/cosmos/base/tendermint/v1beta1/abci_query", header: map[string]string{"Origin": "https://erth.network"}},
	}
	for name, q := range cases {
		w := do(lcd, q)
		if w.Code == http.StatusOK {
			t.Errorf("%s: served (%d) %s", name, w.Code, w.Body.String())
		}
		if got := node.take(); len(got) != 0 {
			t.Errorf("%s: reached the node: %+v", name, got[0])
		}
	}
}

func TestLCDServed(t *testing.T) {
	_, lcd, node := handlers(t)
	jsonPost := func(target, body string) req {
		return req{method: "POST", target: target, body: body, header: map[string]string{"Content-Type": "application/json; charset=utf-8"}}
	}
	hexKey := strings.Repeat("ab", 32)
	cases := []struct {
		q         req
		wantQuery string // "-" = do not check
	}{
		// wallets (iOS, Android)
		{get("/cosmos/auth/v1beta1/accounts/" + addr), ""},
		{jsonPost("/cosmos/tx/v1beta1/txs", `{"tx_bytes":"CgQKAggB","mode":"BROADCAST_MODE_SYNC"}`), ""},
		{jsonPost("/cosmos/tx/v1beta1/simulate", `{"tx_bytes":"CgQKAggB"}`), ""},
		{get("/cosmos/tx/v1beta1/txs/" + txHash), ""},
		{get("/cosmos/bank/v1beta1/balances/" + addr), "pagination.limit=100"},
		{get("/cosmos/bank/v1beta1/supply/by_denom?denom=uerth"), "denom=uerth"},
		{get("/cosmos/bank/v1beta1/supply/by_denom?denom=dexlp%2F1"), "denom=dexlp%2F1"},
		{get("/cosmos/bank/v1beta1/supply/by_denom?denom=dexlp/1"), "denom=dexlp%2F1"},
		{get("/cosmos/staking/v1beta1/validators?status=BOND_STATUS_BONDED&pagination.limit=200"), "pagination.limit=200&status=BOND_STATUS_BONDED"},
		{get("/cosmos/staking/v1beta1/delegations/" + addr), "pagination.limit=100"},
		{get("/cosmos/staking/v1beta1/pool"), ""},
		{get("/cosmos/staking/v1beta1/delegators/" + addr + "/unbonding_delegations"), "pagination.limit=100"},
		{get("/cosmos/staking/v1beta1/params"), ""},
		{get("/cosmos/distribution/v1beta1/delegators/" + addr + "/rewards"), ""},
		{get("/cosmos/gov/v1/proposals?pagination.limit=20&pagination.reverse=true"), "pagination.limit=20&pagination.reverse=true"},
		{get("/cosmos/gov/v1/proposals/3/tally"), ""},
		{get("/cosmos/base/node/v1beta1/config"), ""},
		{get("/cosmos/base/tendermint/v1beta1/blocks/latest"), ""},
		{get("/cosmos/base/tendermint/v1beta1/blocks/1"), ""},
		{get("/cosmos/base/tendermint/v1beta1/node_info"), ""},
		{get("/cosmos/base/tendermint/v1beta1/syncing"), ""},
		{get("/earth/allocation/v1/options/STREAM_ID_CARETAKER"), "pagination.limit=100"},
		{get("/earth/assembly/v1/proposal_tally/4"), ""},
		{get("/earth/assembly/v1/removal_ballots"), ""},
		{get("/earth/assembly/v1/ballot_inputs?proposal_id=4"), "proposal_id=4"},
		{get("/earth/assembly/v1/ballot_inputs?option_id=2"), "option_id=2"},
		{get("/earth/dex/v1/pool"), "pagination.limit=100"},
		{get("/earth/dex/v1/params"), ""},
		{get("/earth/dex/v1/unbondings/" + addr), ""},
		{get("/earth/dex/v1/simulate_swap_exact_in?offer_denom=uerth&offer_amount=1000&ask_denom=uanml"), "-"},
		{get("/earth/personhood/v1/registration_count"), ""},
		{get("/earth/personhood/v1/params"), ""},
		{get("/earth/personhood/v1/lease_bounds"), ""},
		{get("/earth/personhood/v1/handles?start=al%20ice&limit=1000"), "limit=1000&start=al+ice"},
		{req{method: "GET", target: "/earth/personhood/v1/identity_tree", header: map[string]string{"X-Cosmos-Block-Height": "120"}}, ""},
		{get("/earth/shielded/v1/params"), ""},
		{get("/earth/shielded/v1/tree"), ""},
		{get("/earth/shielded/v1/assets?pagination.limit=500&pagination.key=AAEC%2B%2F%3D%3D"), "pagination.key=AAEC%2B%2F%3D%3D&pagination.limit=500"},
		{get("/earth/shielded/v1/nullifiers/" + hexKey), ""},
		{get("/earth/shielded/v1/roots/" + hexKey), ""},
		{get("/earth/shieldedstaking/v1/params"), ""},
		{get("/earth/shieldedstaking/v1/epoch"), ""},
		{get("/earth/shieldedstaking/v1/snapshots/3"), ""},
		{get("/earth/shieldedstaking/v1/stake_nullifier_tree?start=0&limit=1000"), "-"},
		{get("/earth/shieldedstaking/v1/debt_tree?start=10&limit=500"), "-"},
		{get("/earth/shieldedstaking/v1/validators?pagination.limit=200"), ""[:0] + "pagination.limit=200"},
		{get("/earth/shieldedstaking/v1/positions"), "pagination.limit=100"},
		{get("/earth/shieldedstaking/v1/stake_nullifiers/" + hexKey), ""},
		{get("/earth/shieldedstaking/v1/stake_tree"), ""},
		// web app (beyond the wallets')
		{get("/cosmos/tx/v1beta1/txs?query=tx.height%3D123&order_by=ORDER_BY_DESC&limit=50"), "limit=50&order_by=ORDER_BY_DESC&page=1&query=tx.height%3D123"},
		{get("/cosmos/tx/v1beta1/txs?query=tx.height%3D123"), "limit=50&page=1&query=tx.height%3D123"},
		{get("/cosmos/tx/v1beta1/txs?query=tx.height%3D9&limit=7&page=2"), "limit=7&page=2&query=tx.height%3D9"},
		{get("/cosmos/gov/v1/proposals?pagination.offset=10000&pagination.limit=1000&pagination.count_total=false"), "pagination.count_total=false&pagination.limit=1000&pagination.offset=10000"},
		{get("/cosmos/base/tendermint/v1beta1/validatorsets/latest?pagination.limit=200"), "-"},
		{get("/cosmos/bank/v1beta1/send_enabled?denoms=uerth"), "denoms=uerth&pagination.limit=100"},
		{get("/cosmos/bank/v1beta1/params"), ""},
		{get("/cosmos/staking/v1beta1/validators/" + valop), ""},
		{get("/cosmos/staking/v1beta1/validators/" + valop + "/delegations/" + addr), ""},
		{get("/cosmos/staking/v1beta1/validators/" + valop + "/delegations/" + addr + "/unbonding_delegation"), ""},
		{get("/cosmos/staking/v1beta1/validators?pagination.limit=200&pagination.key=abc"), "-"},
		{get("/cosmos/distribution/v1beta1/delegators/" + addr + "/rewards/" + valop), ""},
		{get("/cosmos/distribution/v1beta1/validators/" + valop + "/commission"), ""},
		{get("/cosmos/slashing/v1beta1/params"), ""},
		{get("/cosmos/slashing/v1beta1/signing_infos?pagination.limit=200"), "-"},
		{get("/cosmos/gov/v1/proposals/3/votes/" + addr), ""},
		{get("/cosmos/gov/v1/params/deposit"), ""},
		{get("/earth/allocation/v1/options/STREAM_ID_GROUNDWORKS?pagination.limit=100"), "-"},
		{get("/earth/allocation/v1/params"), ""},
		{get("/earth/allocation/v1/voter/STREAM_ID_GROUNDWORKS/" + addr), ""},
		{get("/earth/dex/v1/pool/1"), ""},
		{get("/earth/dex/v1/liquidity_auction"), ""},
		{get("/earth/dex/v1/liquidity_auction/bid/" + addr), ""},
		{get("/earth/dex/v1/pol_burns"), ""},
		{get("/earth/earth/v1/burns"), ""},
		{get("/earth/personhood/v1/caretaker_voter_count"), ""},
		{get("/earth/personhood/v1/registration_countries"), ""},
		{get("/earth/personhood/v1/registrations_by_dsc/" + hexKey), ""},
		{get("/earth/shielded/v1/turnstiles?pagination.limit=200"), "-"},
		{req{method: "OPTIONS", target: "/cosmos/tx/v1beta1/txs", header: map[string]string{"Origin": "https://erth.network", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type"}}, ""},
		// backend (cosmpy: camelCase JSON)
		{get("/cosmos/bank/v1beta1/balances/" + addr + "/by_denom?denom=uerth"), "denom=uerth"},
		{jsonPost("/cosmos/tx/v1beta1/simulate", `{"tx":{"body":{"messages":[]},"authInfo":{},"signatures":[]}}`), ""},
		{jsonPost("/cosmos/tx/v1beta1/txs", `{"txBytes":"CgQKAggB","mode":"BROADCAST_MODE_SYNC"}`), ""},
		// unknown parameters are dropped, not forwarded
		{get("/cosmos/bank/v1beta1/params?x=%2Fstore%2Fbank"), ""},
	}
	for _, c := range cases {
		w := do(lcd, c.q)
		if w.Code != http.StatusOK {
			t.Errorf("%s %s: %d %s", c.q.method, c.q.target, w.Code, w.Body.String())
			continue
		}
		got := node.take()
		if len(got) != 1 {
			t.Errorf("%s: node saw %d", c.q.target, len(got))
			continue
		}
		g := got[0]
		path := strings.SplitN(c.q.target, "?", 2)[0]
		if g.method != c.q.method || g.path != path {
			t.Errorf("%s: node got %s %s", c.q.target, g.method, g.path)
		}
		if c.wantQuery != "-" && g.rawQuery != c.wantQuery {
			t.Errorf("%s: node got query %q, want %q", c.q.target, g.rawQuery, c.wantQuery)
		}
		if c.q.method == "POST" && g.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: content type %q", c.q.target, g.header.Get("Content-Type"))
		}
		if h := c.q.header["X-Cosmos-Block-Height"]; h != "" && g.header.Get("X-Cosmos-Block-Height") != h {
			t.Errorf("%s: height header not forwarded", c.q.target)
		}
		if w.Header().Get("Grpc-Metadata-X-Cosmos-Block-Height") != "7" {
			t.Errorf("%s: answer's height header dropped", c.q.target)
		}
	}
}

// abci_query serves its own short list, all in the abci-query class, and none of
// the LCD's paginated methods, nor anything from the tx service.
func TestABCIGRPCList(t *testing.T) {
	c := DefaultClasses()
	for p := range abciGRPC {
		cl, err := checkABCIQuery(c, p, nil, false)
		if err != nil || cl != c.abciQuery {
			t.Errorf("%s: %v (class %v)", p, err, cl)
		}
		if strings.HasPrefix(p, "/cosmos.tx.") {
			t.Errorf("tx service method %s served over abci_query", p)
		}
	}
	for _, s := range lcdSpecs {
		if s.page && abciGRPC[s.grpc] {
			t.Errorf("paginated %s served over abci_query", s.grpc)
		}
	}
	for _, p := range []string{"/cosmos.tx.v1beta1.Service/Simulate", "/cosmos.bank.v1beta1.Query/AllBalances", "/earth.earth.v1.Query/Burns"} {
		if _, err := checkABCIQuery(c, p, nil, false); err == nil {
			t.Errorf("%s served over abci_query", p)
		}
	}
}

func TestBusy(t *testing.T) {
	block := make(chan struct{})
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer node.Close()
	defer close(block)
	c := DefaultClasses()
	c.search.wait = 0
	lcd := NewLCD(node.URL, &http.Client{Transport: NewTransport()}, c)
	q := get("/cosmos/tx/v1beta1/txs?query=tx.height%3D5")
	go do(lcd, q) // holds the one search slot
	for used(c.search) == 0 {
		// wait for the first to take its slot
	}
	if w := do(lcd, q); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("second concurrent search: %d, want 503", w.Code)
	}
}
