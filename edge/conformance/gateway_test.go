package conformance

import (
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/protoc-gen-grpc-gateway/httprule"
	"github.com/grpc-ecosystem/grpc-gateway/runtime"

	"github.com/zenopie/earth-network-deploy/edge/filter"
)

// gwRoute is one HTTP binding from a proto's google.api.http option, as the
// gateway compiles it.
type gwRoute struct {
	method, template, file string
	pat                    runtime.Pattern
}

// A binding's value can be adjacent string literals, protobuf's
// concatenation: "/cosmos/staking/v1beta1/validators/{validator_addr}/delegations/" "{delegator_addr}".
var (
	reBinding = regexp.MustCompile(`\b(get|post)\s*[:=]\s*((?:"[^"]*"\s*)+)`)
	reLiteral = regexp.MustCompile(`"([^"]*)"`)
)

func loadGateway(t testing.TB) []gwRoute {
	var out []gwRoute
	for _, dir := range protoDirs(t) {
		err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".proto") {
				return err
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			// Bindings can span lines: `option (google.api.http).get =\n "/..."`.
			for _, m := range reBinding.FindAllStringSubmatch(string(src), -1) {
				var tmpl strings.Builder
				for _, l := range reLiteral.FindAllStringSubmatch(m[2], -1) {
					tmpl.WriteString(l[1])
				}
				m[2] = tmpl.String()
				c, err := httprule.Parse(m[2])
				if err != nil {
					// protoc-gen-grpc-gateway refuses it too, so no
					// handler exists (reflection's "/interfaces/").
					continue
				}
				tm := c.Compile()
				pat, err := runtime.NewPattern(1, tm.OpCodes, tm.Pool, tm.Verb)
				if err != nil {
					t.Fatalf("%s: %s: %v", p, m[2], err)
				}
				out = append(out, gwRoute{strings.ToUpper(m[1]), m[2], p, pat})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) < 100 {
		t.Fatalf("only %d gateway bindings found", len(out))
	}
	return out
}

// gwMatches: the gateway templates that would serve this method and path
// (runtime/mux.go ServeHTTP, for a path with no ":verb", which the filter
// never passes), in proto order.
func gwMatches(gw []gwRoute, method, path string) []gwRoute {
	components := strings.Split(path[1:], "/")
	var out []gwRoute
	for _, r := range gw {
		if r.method != method {
			continue
		}
		if _, err := r.pat.Match(components, ""); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// servedBy: the gateway serves the path with the filter's route. One
// binding matches, or several from one service, where the generated code
// registers them in proto order and the first match wins (runtime/mux.go
// Handle appends; ServeHTTP takes the first): cmtservice's ".../latest"
// before ".../{height}".
func servedBy(m []gwRoute, pattern string) bool {
	if len(m) == 0 || shape(m[0].template) != shape(pattern) {
		return false
	}
	for _, r := range m[1:] {
		if r.file != m[0].file {
			return false
		}
	}
	return true
}

func templates(m []gwRoute) []string {
	var out []string
	for _, r := range m {
		out = append(out, r.template)
	}
	return out
}

var reVar = regexp.MustCompile(`\{[^}]*\}`)

func shape(p string) string { return reVar.ReplaceAllString(p, "{}") }

var samples = map[string]string{
	"{addr}": "earth1qyqszqgpqyqszqgpqyqszqgpqyqszqgp5f3k9z",
	"{uint}": "5",
	"{hash}": strings.Repeat("AB", 32),
	"{hex}":  strings.Repeat("ab", 32),
	"{name}": "STREAM_ID_CARETAKER",
}

func sample(p string) string {
	return reVar.ReplaceAllStringFunc(p, func(v string) string { return samples[v] })
}

// Every served route is a real gateway binding, and a path the filter
// accepts for it is served by that binding: the node runs the method the
// filter checked.
func TestRoutesAreGatewayBindings(t *testing.T) {
	gw := loadGateway(t)
	for _, r := range filter.LCDRoutes() {
		p := sample(r.Pattern)
		got, ok := filter.MatchLCDPath(r.Method, p)
		if !ok || got != r.Pattern {
			t.Errorf("%s %s: sample %s matched %q", r.Method, r.Pattern, p, got)
			continue
		}
		if m := gwMatches(gw, r.Method, p); !servedBy(m, r.Pattern) {
			t.Errorf("%s %s: gateway bindings matching %s: %v", r.Method, r.Pattern, p, templates(m))
		}
	}
}

// Mutated paths: whenever the filter accepts one, the gateway serves it
// with the route the filter matched.
func TestMutatedPaths(t *testing.T) {
	gw := loadGateway(t)
	routes := filter.LCDRoutes()
	rng := rand.New(rand.NewSource(1))
	pieces := []string{"", ".", "..", "%2F", "%2f", ":x", "latest", "block", "5", "-1",
		"abci_query", "txs", "x/y", "earth1", "AB", " ", "%00", "~", "a.b", "STREAM_ID_X"}
	accepted := 0
	for i := 0; i < 200000; i++ {
		r := routes[rng.Intn(len(routes))]
		segs := strings.Split(sample(r.Pattern)[1:], "/")
		for j := range segs {
			if rng.Intn(4) == 0 {
				segs[j] = pieces[rng.Intn(len(pieces))]
			}
		}
		if rng.Intn(8) == 0 {
			segs = append(segs, pieces[rng.Intn(len(pieces))])
		}
		if rng.Intn(8) == 0 && len(segs) > 1 {
			segs = segs[:len(segs)-1]
		}
		p := "/" + strings.Join(segs, "/")
		got, ok := filter.MatchLCDPath(r.Method, p)
		if !ok {
			continue
		}
		accepted++
		if m := gwMatches(gw, r.Method, p); !servedBy(m, got) {
			t.Fatalf("%s %s: filter matched %s, gateway bindings %v", r.Method, p, got, templates(m))
		}
	}
	if accepted < 1000 {
		t.Fatalf("only %d mutated paths accepted; the test is not exercising anything", accepted)
	}
}

func FuzzLCDPath(f *testing.F) {
	gw := loadGateway(f)
	for _, r := range filter.LCDRoutes() {
		f.Add(r.Method == "POST", sample(r.Pattern))
	}
	f.Fuzz(func(t *testing.T, isPost bool, p string) {
		method := "GET"
		if isPost {
			method = "POST"
		}
		got, ok := filter.MatchLCDPath(method, p)
		if !ok {
			return
		}
		if strings.ContainsAny(p, "%:?#") || strings.Contains(p, "//") || strings.HasSuffix(p, "/") {
			t.Fatalf("accepted an ambiguous path %q", p)
		}
		if m := gwMatches(gw, method, p); !servedBy(m, got) {
			t.Fatalf("%s %s: filter matched %s, gateway bindings %v", method, p, got, templates(m))
		}
	})
}
