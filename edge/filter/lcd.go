package filter

// The LCD side (lcd.erth.network -> node:1317).
//
// How the node routes (cosmos-sdk v0.53 server/api, grpc-gateway v1.16.0
// runtime/mux.go, gorilla/mux v1.8.1):
//
//   - gorilla/mux cleans r.URL.Path (301 for "//", "/./", "/../") and hands
//     everything to the gateway, after the grpc-web handler (a POST with
//     Content-Type application/grpc-web* reaches the gRPC server directly:
//     every service, cosmos.tx.v1beta1.Service/GetTxsEvent included).
//   - The gateway splits the DECODED path on "/" and takes a trailing
//     ":verb" off the last segment; a pattern's {var} matches any one
//     segment, empty included, and {var=**} the rest. So "%2F" in a segment
//     is a slash to the router, and a trailing slash is an empty {var}.
//   - X-HTTP-Method-Override, or no override at all, turns a POST with
//     Content-Type exactly application/x-www-form-urlencoded into whatever
//     GET matches the path, its parameters read from the body.
//   - Query parameters (and form fields) fill the request message by proto
//     or JSON field name, dotted for nested fields; unknown names are ignored.
//   - The x-cosmos-block-height header picks the query height.
//
// So this side serves only: GET on an allowlisted route, POST of a JSON
// object to broadcast or simulate, and CORS preflights for those. The path
// must contain only [A-Za-z0-9._~/-] in non-empty segments that are not "."
// or ".." (no %-escape, no ":verb", no "//", no trailing slash): then the
// path the gateway routes on is byte-for-byte the one matched here, and each
// {var} is checked against what that field can hold. Query parameters are
// filtered to the route's own and re-encoded; the request is rebuilt with
// only the headers in clientHeaders.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

type segKind int

const (
	segLit segKind = iota
	segVar
	segRest // {x=**}: one or more segments, last in the pattern
)

type seg struct {
	kind segKind
	lit  string
	re   *regexp.Regexp
}

type lcdRoute struct {
	method  string // GET or POST
	pattern string
	segs    []seg
	params  map[string]*regexp.Regexp // query parameters served, by name
	body    map[string]bool           // POST: the JSON keys allowed
	class   func(*Classes) *class
	// search: the route is the tx search; its query is checked by checkSearch.
	search bool
	grpc   string // the gRPC method behind it ("" for the tx service)
	page   bool   // its request has a PageRequest
}

// Path parameter types, by the name used in lcdpolicy.go's patterns.
var segTypes = map[string]*regexp.Regexp{
	"addr":  regexp.MustCompile(`^[a-z0-9]{1,16}1[02-9ac-hj-np-z]{6,90}$`), // bech32, lower case
	"hash":  regexp.MustCompile(`^[0-9A-Fa-f]{64}$`),
	"uint":  regexp.MustCompile(`^[0-9]{1,20}$`),
	"denom": regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9.\-_]{1,127}$`),
	"name":  regexp.MustCompile(`^[a-zA-Z0-9_\-.]{1,128}$`),
	"hex":   regexp.MustCompile(`^[0-9A-Fa-f]{2,512}$`),
	"b64u":  regexp.MustCompile(`^[A-Za-z0-9_\-=]{1,512}$`),
	// {denom=**}: an IBC or factory denom's segments, each a denom part.
	"denoms": regexp.MustCompile(`^[a-zA-Z0-9.\-_]{1,128}$`),
}

var errPattern = errors.New("bad pattern")

func compilePattern(p string) ([]seg, error) {
	if !strings.HasPrefix(p, "/") {
		return nil, errPattern
	}
	parts := strings.Split(p[1:], "/")
	var out []seg
	for i, part := range parts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			name := part[1 : len(part)-1]
			typ := name
			if j := strings.IndexByte(name, ':'); j >= 0 {
				typ = name[j+1:]
			}
			re, ok := segTypes[typ]
			if !ok {
				return nil, errPattern
			}
			k := segVar
			if typ == "denoms" {
				if i != len(parts)-1 {
					return nil, errPattern
				}
				k = segRest
			}
			out = append(out, seg{kind: k, re: re})
			continue
		}
		if part == "" {
			return nil, errPattern
		}
		out = append(out, seg{kind: segLit, lit: part})
	}
	return out, nil
}

func (rt *lcdRoute) match(parts []string) bool {
	for i, s := range rt.segs {
		switch s.kind {
		case segLit:
			if i >= len(parts) || parts[i] != s.lit {
				return false
			}
		case segVar:
			if i >= len(parts) || !s.re.MatchString(parts[i]) {
				return false
			}
		case segRest:
			if i >= len(parts) {
				return false
			}
			for _, p := range parts[i:] {
				if !s.re.MatchString(p) {
					return false
				}
			}
			return true
		}
	}
	return len(parts) == len(rt.segs)
}

var lcdPathChars = regexp.MustCompile(`^(/[A-Za-z0-9._~\-]+)+$`)

// splitLCDPath: the path's segments, or false if the path is not one the
// gateway would route exactly as written.
func splitLCDPath(r *http.Request) ([]string, bool) {
	if !cleanRawPath(r) {
		return nil, false
	}
	p := r.URL.Path
	if !lcdPathChars.MatchString(p) {
		return nil, false // also: "//", trailing "/", ":verb", anything escaped
	}
	parts := strings.Split(p[1:], "/")
	for _, s := range parts {
		if s == "." || s == ".." {
			return nil, false
		}
	}
	return parts, true
}

type lcdHandler struct {
	up      *nodeClient
	classes *Classes
	routes  []*lcdRoute
}

func (h *lcdHandler) find(method string, parts []string) *lcdRoute {
	for _, rt := range h.routes {
		if (method == "" || rt.method == method) && rt.match(parts) {
			return rt
		}
	}
	return nil
}

func (h *lcdHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isUpgrade(r) {
		lcdRefuse(w, http.StatusForbidden, "websockets are not served")
		return
	}
	if r.Header.Get("X-HTTP-Method-Override") != "" {
		lcdRefuse(w, http.StatusForbidden, "method override is not served")
		return
	}
	parts, ok := splitLCDPath(r)
	if !ok {
		lcdRefuse(w, http.StatusForbidden, "percent-encoded or malformed path")
		return
	}
	switch r.Method {
	case http.MethodOptions:
		rt := h.find("", parts)
		if rt == nil || !isPreflight(r) {
			lcdRefuse(w, http.StatusForbidden, "OPTIONS is served as a CORS preflight for a served route only")
			return
		}
		h.up.forward(w, r, h.classes.light, http.MethodOptions, r.URL.Path, nil, clientHeaders(r), fwdOpts{})
	case http.MethodGet:
		rt := h.find(http.MethodGet, parts)
		if rt == nil {
			lcdRefuse(w, http.StatusForbidden, "route not served")
			return
		}
		if hasBody(r) {
			lcdRefuse(w, http.StatusForbidden, "body on a GET")
			return
		}
		q, err := filterQuery(rt, r.URL.RawQuery)
		if err != nil {
			lcdRefuse(w, http.StatusForbidden, err.Error())
			return
		}
		cl := rt.class(h.classes)
		if rt.search {
			if err := checkSearch(q); err != nil {
				lcdRefuse(w, http.StatusForbidden, err.Error())
				return
			}
		}
		if rt.page && q.Get("pagination.limit") == "" {
			// An absent (or zero) limit makes the SDK page 100 AND count
			// the whole collection (CountTotal): every default page a full
			// walk (R5-E-4). The limit is always explicit, 1..1000.
			q.Set("pagination.limit", defaultPageLimit)
		}
		hdr := clientHeaders(r)
		if v := r.Header.Get("X-Cosmos-Block-Height"); v != "" {
			if !segTypes["uint"].MatchString(v) {
				lcdRefuse(w, http.StatusForbidden, "bad x-cosmos-block-height")
				return
			}
			hdr.Set("X-Cosmos-Block-Height", v)
		}
		pq := r.URL.Path
		if enc := q.Encode(); enc != "" {
			pq += "?" + enc
		}
		h.up.forward(w, r, cl, http.MethodGet, pq, nil, hdr, fwdOpts{})
	case http.MethodPost:
		rt := h.find(http.MethodPost, parts)
		if rt == nil {
			lcdRefuse(w, http.StatusForbidden, "route not served")
			return
		}
		if r.URL.RawQuery != "" {
			lcdRefuse(w, http.StatusForbidden, "query string on a POST")
			return
		}
		ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if ct != "application/json" && !strings.HasPrefix(ct, "application/json;") {
			lcdRefuse(w, http.StatusUnsupportedMediaType, "POST bodies are application/json")
			return
		}
		body, release, code := readBody(w, r, factorLCD, h.classes.isBackend(r))
		if code != 0 {
			lcdRefuse(w, code, http.StatusText(code))
			return
		}
		defer release()
		if err := checkJSONObject(body, rt.body); err != nil {
			lcdRefuse(w, http.StatusBadRequest, err.Error())
			return
		}
		hdr := clientHeaders(r)
		hdr.Set("Content-Type", "application/json")
		h.up.forward(w, r, rt.class(h.classes), http.MethodPost, r.URL.Path, body, hdr, fwdOpts{})
	default:
		lcdRefuse(w, http.StatusMethodNotAllowed, "method not served")
	}
}

// Pagination, on every route that takes it (cosmos.base.query.v1beta1.PageRequest).
// The SDK has no maximum of its own; each page is bounded here (R5-E-4):
//   - limit 1..1000, injected as 100 when absent (0 or absent turns on
//     CountTotal, a walk of the whole collection);
//   - offset at most 10,000 (the node iterates past every skipped entry;
//     clients page with pagination.key, which seeks);
//   - count_total=true refused (no client asks for it; it walks the lot).
var paginationParams = map[string]*regexp.Regexp{
	"pagination.key":         regexp.MustCompile(`^[A-Za-z0-9+/_\-=]{0,512}$`),
	"pagination.offset":      regexp.MustCompile(`^([0-9]{1,4}|10000)$`),
	"pagination.limit":       regexp.MustCompile(`^([1-9][0-9]{0,2}|1000)$`),
	"pagination.count_total": regexp.MustCompile(`^false$`),
	"pagination.reverse":     regexp.MustCompile(`^(true|false)$`),
}

const defaultPageLimit = "100"

// filterQuery keeps the route's own parameters, first value each, and
// refuses a kept value its pattern does not allow. Unknown names are
// dropped: the gateway ignores them anyway, so dropping changes nothing for
// a real client and leaves nothing for a decoy.
func filterQuery(rt *lcdRoute, raw string) (url.Values, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, errors.New("malformed query string")
	}
	out := url.Values{}
	for name, vs := range q {
		re, ok := rt.params[name]
		if !ok {
			continue
		}
		if len(vs) != 1 {
			return nil, errors.New("repeated query parameter " + name)
		}
		if !re.MatchString(vs[0]) {
			return nil, errors.New("query parameter " + name + " not allowed with that value")
		}
		out.Set(name, vs[0])
	}
	return out, nil
}

// checkJSONObject: body is one JSON object (no trailing data) with only
// the allowed keys, each once.
func checkJSONObject(body []byte, allowed map[string]bool) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return errors.New("body must be a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return errors.New("body must be a JSON object")
		}
		k, _ := t.(string)
		if !allowed[k] || seen[k] {
			return errors.New("body field " + k + " not served")
		}
		seen[k] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return errors.New("body must be a JSON object")
		}
	}
	if _, err := dec.Token(); err != nil {
		return errors.New("body must be a JSON object")
	}
	if _, err := dec.Token(); err == nil {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

// lcdRefuse answers in the gateway's error shape (code 7 PermissionDenied).
func lcdRefuse(w http.ResponseWriter, code int, msg string) {
	b, _ := json.Marshal(map[string]interface{}{
		"code":    7,
		"message": "refused by the edge filter: " + msg,
		"details": []interface{}{},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}
