package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zenopie/earth-network-deploy/edge/filter"
)

// What the protos say about each request message: the gRPC method's request
// type, and whether that type has a cosmos.base.query.v1beta1.PageRequest
// field. Read from the same proto trees as the gateway bindings.
var (
	rePackage = regexp.MustCompile(`(?m)^\s*package\s+([\w.]+)\s*;`)
	reService = regexp.MustCompile(`\bservice\s+(\w+)\s*\{`)
	reRPC     = regexp.MustCompile(`\brpc\s+(\w+)\s*\(\s*(?:stream\s+)?([\w.]+)\s*\)`)
	reMessage = regexp.MustCompile(`\bmessage\s+(\w+)\s*\{`)
	rePage    = regexp.MustCompile(`(?m)^\s*(?:cosmos\.base\.query\.v1beta1\.)?PageRequest\s+\w+\s*=\s*\d+`)
)

// block returns the text between the brace at open and its match.
func block(src string, open int) string {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[open+1 : i]
			}
		}
	}
	return src[open+1:]
}

// topLevel strips nested message/enum/oneof-free blocks so a nested
// message's PageRequest is not counted as the request's own.
func topLevel(body string) string {
	var b strings.Builder
	depth := 0
	for _, r := range body {
		switch r {
		case '{':
			depth++
			continue
		case '}':
			depth--
			continue
		}
		if depth == 0 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

type protoIndex struct {
	methods map[string]string // "/pkg.Service/Method" -> "pkg.Request"
	paged   map[string]bool   // "pkg.Message" -> has a top-level PageRequest field
	seen    map[string]bool   // every message
}

func loadProtoIndex(t *testing.T) protoIndex {
	ix := protoIndex{methods: map[string]string{}, paged: map[string]bool{}, seen: map[string]bool{}}
	for _, dir := range protoDirs(t) {
		err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".proto") {
				return err
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), "")
			pm := rePackage.FindStringSubmatch(src)
			if pm == nil {
				return nil
			}
			pkg := pm[1]
			for _, m := range reService.FindAllStringSubmatchIndex(src, -1) {
				svc := src[m[2]:m[3]]
				body := block(src, m[1]-1)
				for _, r := range reRPC.FindAllStringSubmatch(body, -1) {
					req := r[2]
					if !strings.Contains(req, ".") {
						req = pkg + "." + req
					}
					ix.methods["/"+pkg+"."+svc+"/"+r[1]] = req
				}
			}
			for _, m := range reMessage.FindAllStringSubmatchIndex(src, -1) {
				name := pkg + "." + src[m[2]:m[3]]
				if ix.seen[name] {
					continue // a nested message of the same name; top-level wins (first)
				}
				ix.seen[name] = true
				ix.paged[name] = rePage.MatchString(topLevel(block(src, m[1]-1)))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(ix.methods) < 100 {
		t.Fatalf("only %d gRPC methods found", len(ix.methods))
	}
	return ix
}

// Every paginated LCD route is one whose request takes a PageRequest, and
// every route whose request takes one is marked paginated: the filter then
// injects and bounds pagination.limit exactly where the node pages (R5-E-4;
// Burns, PolBurns and RegistrationsByDsc were marked paginated without a
// PageRequest).
func TestPaginationMatchesProtos(t *testing.T) {
	ix := loadProtoIndex(t)
	for _, r := range filter.LCDRoutes() {
		if r.GRPC == "" {
			continue
		}
		req, ok := ix.methods[r.GRPC]
		if !ok {
			t.Errorf("%s: gRPC method %s not in the protos", r.Pattern, r.GRPC)
			continue
		}
		if !ix.seen[req] {
			t.Errorf("%s: request type %s not found", r.GRPC, req)
			continue
		}
		if ix.paged[req] != r.Paginated {
			t.Errorf("%s (%s): request has PageRequest %v, filter says paginated %v", r.Pattern, req, ix.paged[req], r.Paginated)
		}
	}
}

// Nothing served over abci_query takes a PageRequest: those calls hold the
// node's ABCI mutex, and their protobuf data is not decoded by the filter,
// so a page there would be bounded by nothing but query gas (R5-E-3).
func TestABCIPathsUnpaginated(t *testing.T) {
	ix := loadProtoIndex(t)
	for _, p := range filter.ABCIGRPCPaths() {
		req, ok := ix.methods[p]
		if !ok {
			t.Errorf("abci_query path %s is not a gRPC method in the protos", p)
			continue
		}
		if ix.paged[req] {
			t.Errorf("abci_query path %s takes a PageRequest (%s)", p, req)
		}
	}
}
