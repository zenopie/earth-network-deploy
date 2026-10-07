package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	cmtbytes "github.com/cometbft/cometbft/libs/bytes"
	"github.com/cometbft/cometbft/libs/log"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	rpcserver "github.com/cometbft/cometbft/rpc/jsonrpc/server"
	rpctypes "github.com/cometbft/cometbft/rpc/jsonrpc/types"
	cmttypes "github.com/cometbft/cometbft/types"

	"github.com/zenopie/earth-network-deploy/edge/filter"
)

// One CometBFT route per argument kind, each recording what the node's own
// decoder made of its argument "a".
var (
	mu  sync.Mutex
	got interface{}
)

func record(v interface{}) (*ctypes.ResultHealth, error) {
	mu.Lock()
	got = v
	mu.Unlock()
	return &ctypes.ResultHealth{}, nil
}

var kinds = map[string]filter.ArgKind{
	"k_string": filter.KString, "k_hexbytes": filter.KHexBytes, "k_bytes": filter.KBytes,
	"k_tx": filter.KBytes, "k_int64": filter.KInt64, "k_int64ptr": filter.KInt64Ptr,
	"k_intptr": filter.KIntPtr, "k_uint": filter.KUint, "k_bool": filter.KBool,
}

// node is CometBFT's own HTTP stack for these routes, called in process.
type nodeH struct{ http.Handler }

func node(t testing.TB) nodeH {
	routes := map[string]*rpcserver.RPCFunc{
		"k_string":   rpcserver.NewRPCFunc(func(_ *rpctypes.Context, a string) (*ctypes.ResultHealth, error) { return record(a) }, "a"),
		"k_hexbytes": rpcserver.NewRPCFunc(func(_ *rpctypes.Context, a cmtbytes.HexBytes) (*ctypes.ResultHealth, error) { return record([]byte(a)) }, "a"),
		"k_bytes":    rpcserver.NewRPCFunc(func(_ *rpctypes.Context, a []byte) (*ctypes.ResultHealth, error) { return record(a) }, "a"),
		"k_tx":       rpcserver.NewRPCFunc(func(_ *rpctypes.Context, a cmttypes.Tx) (*ctypes.ResultHealth, error) { return record([]byte(a)) }, "a"),
		"k_int64":    rpcserver.NewRPCFunc(func(_ *rpctypes.Context, a int64) (*ctypes.ResultHealth, error) { return record(a) }, "a"),
		"k_int64ptr": rpcserver.NewRPCFunc(func(_ *rpctypes.Context, a *int64) (*ctypes.ResultHealth, error) { return record(a) }, "a"),
		"k_intptr":   rpcserver.NewRPCFunc(func(_ *rpctypes.Context, a *int) (*ctypes.ResultHealth, error) { return record(a) }, "a"),
		"k_uint":     rpcserver.NewRPCFunc(func(_ *rpctypes.Context, a uint) (*ctypes.ResultHealth, error) { return record(a) }, "a"),
		"k_bool":     rpcserver.NewRPCFunc(func(_ *rpctypes.Context, a bool) (*ctypes.ResultHealth, error) { return record(a) }, "a"),
	}
	mux := http.NewServeMux()
	rpcserver.RegisterRPCFuncs(mux, routes, log.NewNopLogger())
	cfg := rpcserver.DefaultConfig()
	return nodeH{rpcserver.PreChecksHandler(rpcserver.RecoverAndLogHandler(mux, log.NewNopLogger()), cfg)}
}

// result is a decoded value in one comparable shape.
type result struct {
	ok  bool
	str string // the value, printed
}

func show(v interface{}) string {
	switch x := v.(type) {
	case []byte:
		return fmt.Sprintf("bytes:%x", x)
	case *int64:
		if x == nil {
			return "nil"
		}
		return fmt.Sprintf("int:%d", *x)
	case *int:
		if x == nil {
			return "nil"
		}
		return fmt.Sprintf("int:%d", *x)
	case int64:
		return fmt.Sprintf("int:%d", x)
	case uint:
		return fmt.Sprintf("int:%d", x)
	case string:
		return fmt.Sprintf("str:%q", x)
	case bool:
		return fmt.Sprintf("bool:%v", x)
	}
	return fmt.Sprintf("?%T", v)
}

func showFilter(k filter.ArgKind, v filter.ArgVal) string {
	set, s, b, i, t := v.Value()
	switch k {
	case filter.KString:
		return fmt.Sprintf("str:%q", s)
	case filter.KHexBytes, filter.KBytes:
		return fmt.Sprintf("bytes:%x", b)
	case filter.KInt64Ptr, filter.KIntPtr:
		if !set {
			return "nil"
		}
		return fmt.Sprintf("int:%d", i)
	case filter.KInt64, filter.KUint:
		return fmt.Sprintf("int:%d", i)
	case filter.KBool:
		return fmt.Sprintf("bool:%v", t)
	}
	return "?"
}

func call(t testing.TB, srv nodeH, req *http.Request) result {
	mu.Lock()
	got = nil
	mu.Unlock()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Error *json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(body, &r)
	mu.Lock()
	defer mu.Unlock()
	if resp.StatusCode != 200 || r.Error != nil || got == nil {
		return result{}
	}
	return result{ok: true, str: show(got)}
}

func cometURI(t testing.TB, srv nodeH, route, value string) result {
	q := ""
	if value != "" {
		q = "?a=" + url.QueryEscape(value)
	}
	req := httptest.NewRequest("GET", "http://node"+"/"+route+q, nil)
	return call(t, srv, req)
}

func cometJSON(t testing.TB, srv nodeH, route string, raw []byte) result {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"` + route + `","params":{"a":`)
	body = append(append(body, raw...), '}', '}')
	req := httptest.NewRequest("POST", "http://node"+"/", bytes.NewReader(body))
	return call(t, srv, req)
}

func filterURI(k filter.ArgKind, value string) result {
	if value == "" {
		return result{ok: true, str: showFilter(k, filter.ArgVal{})}
	}
	v, err := filter.DecodeURIArg(k, value)
	if err != nil {
		return result{}
	}
	return result{ok: true, str: showFilter(k, v)}
}

// filterJSON takes the value out of a params map as the filter's
// parseJSONRPC (and the node's mapParamsToArgs) does: a json.RawMessage
// holds the value's own bytes, without surrounding whitespace.
func filterJSON(k filter.ArgKind, raw []byte) result {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(append(append([]byte(`{"a":`), raw...), '}'), &m); err != nil {
		return result{}
	}
	if len(m["a"]) == 0 {
		return result{ok: true, str: showFilter(k, filter.ArgVal{})}
	}
	v, err := filter.DecodeJSONArg(k, m["a"])
	if err != nil {
		return result{}
	}
	return result{ok: true, str: showFilter(k, v)}
}

// The values that matter: every branch of _nonJSONStringToArg and of
// cmtjson's decoder, the R4-E-1 encodings, and their near misses.
var corpus = []string{
	"", `""`, `"abc"`, "abc", "0x", "0X", "0x00", "0X0a", "0xZZ", "0x1", "0x2f73746f7265",
	"0x" + strings.Repeat("ab", 40), "123", "-5", "00012", "+5", "5.0", "1e3", "1_000",
	`"5"`, `" 5"`, `"5 "`, `"5"`, `"-5"`, `"0x10"`, "null", `"null"`, "true", "false",
	" true", "true ", "\ttrue", "TRUE", `"true"`, "1", "0", "-0",
	"9223372036854775807", "9223372036854775808", "-9223372036854775808", "18446744073709551615",
	`"`, `""""`, `"store/bank/subspace"`, `"/cosmos.tx.v1beta1.Service/GetTxsEvent"`,
	`"/store/bank/subspace"`, `"/cosmos.bank.v1beta1.Query/SupplyOf"`, "QUJD", `"QUJD"`, `"QUJD="`,
	`"ABCD"`, `"abcd"`, `"0A0B"`, `"0a0b"`, `"zz"`, "{}", "[]", `["a"]`, `{"a":1}`,
	`"\ud800"`, "\xff", `"\xff"`, `"a\"b"`, " ", "\x00",
}

func TestCometBFTDecodingURI(t *testing.T) {
	srv := node(t)
	for route, k := range kinds {
		for _, v := range corpus {
			if v == "" {
				continue // absent: both use the zero value
			}
			want := cometURI(t, srv, route, v)
			have := filterURI(k, v)
			if want != have && !allowedStricter(k, want, have) {
				t.Errorf("%s URI %q: node %+v, filter %+v", route, v, want, have)
			}
		}
	}
}

func TestCometBFTDecodingJSON(t *testing.T) {
	srv := node(t)
	for route, k := range kinds {
		for _, v := range corpus {
			raw := []byte(v)
			if !json.Valid(raw) {
				continue // the node refuses the whole body before decoding
			}
			want := cometJSON(t, srv, route, raw)
			have := filterJSON(k, raw)
			if want != have {
				t.Errorf("%s JSON %s: node %+v, filter %+v", route, v, want, have)
			}
		}
	}
}

// allowedStricter: the one place the filter refuses what the node accepts
// (never the reverse): a uint above 2^62. The only uint argument served is
// genesis_chunked's chunk, and the genesis has one.
func allowedStricter(k filter.ArgKind, node, flt result) bool {
	return k == filter.KUint && node.ok && !flt.ok
}

// What the filter forwards decodes, at the node, to what the filter checked.
func TestCanonicalForwarding(t *testing.T) {
	srv := node(t)
	for route, k := range kinds {
		for _, v := range corpus {
			if v == "" {
				continue
			}
			d, err := filter.DecodeURIArg(k, v)
			if err != nil {
				continue
			}
			checkForward(t, srv, route, k, d)
		}
	}
}

func checkForward(t testing.TB, srv nodeH, route string, k filter.ArgKind, d filter.ArgVal) {
	set, _, _, _, _ := d.Value()
	if !set {
		return // not forwarded at all: the node uses the zero value
	}
	want := result{ok: true, str: showFilter(k, d)}
	req := httptest.NewRequest("GET", "http://node"+"/"+route+"?a="+filter.EncodeURI(k, d), nil)
	if got := call(t, srv, req); got != want {
		t.Errorf("%s: URI-forwarded %q decodes to %+v, filter checked %+v", route, filter.EncodeURI(k, d), got, want)
	}
	if _, str, _, _, _ := d.Value(); k == filter.KString && !utf8.ValidString(str) {
		// Only a URI hex value can decode to invalid UTF-8, and a URI call
		// is forwarded as a URI call; JSON strings are valid by decoding.
		return
	}
	if got := cometJSON(t, srv, route, filter.EncodeJSON(k, d)); got != want {
		t.Errorf("%s: JSON-forwarded %s decodes to %+v, filter checked %+v", route, filter.EncodeJSON(k, d), got, want)
	}
}

func FuzzCometBFTDecoding(f *testing.F) {
	for i, v := range corpus {
		f.Add(uint8(i), v)
	}
	srv := node(f)
	routes := make([]string, 0, len(kinds))
	for r := range kinds {
		routes = append(routes, r)
	}
	f.Fuzz(func(t *testing.T, which uint8, v string) {
		route := routes[int(which)%len(routes)]
		k := kinds[route]
		if v != "" && !strings.ContainsRune(v, 0) {
			if want, have := cometURI(t, srv, route, v), filterURI(k, v); want != have && !allowedStricter(k, want, have) {
				t.Errorf("%s URI %q: node %+v, filter %+v", route, v, want, have)
			}
			if d, err := filter.DecodeURIArg(k, v); err == nil {
				checkForward(t, srv, route, k, d)
			}
		}
		if raw := []byte(v); json.Valid(raw) {
			if want, have := cometJSON(t, srv, route, raw), filterJSON(k, raw); want != have {
				t.Errorf("%s JSON %s: node %+v, filter %+v", route, v, want, have)
			}
		}
	})
}
