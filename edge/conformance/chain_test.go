package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The chain these tests read is the commit chain.pin names, materialised by
// fetch-chain.sh into .chain/ (go.mod, proto/, networks/genesis.json and
// app/resultcap/, from git archive).

const chainDir = ".chain"

var reRequire = func(mod string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*(?:require\s+)?` + regexp.QuoteMeta(mod) + `\s+(v\S+)`)
}

func pinnedCommit(t testing.TB) string {
	raw, err := os.ReadFile("chain.pin")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^commit=([0-9a-f]{40})$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("chain.pin names no 40-hex commit")
	}
	return string(m[1])
}

// chainGoMod: .chain/go.mod, after checking that .chain is the pinned
// commit. Missing: skip (fail under CI). Stale: fail, always, since it would
// check the filter against a chain this deployment does not run.
func chainGoMod(t testing.TB) []byte {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(chainDir, "COMMIT"))
	if err != nil {
		skipOutsideCI(t, "no .chain: run ./fetch-chain.sh")
	}
	if want := pinnedCommit(t); strings.TrimSpace(string(got)) != want {
		t.Fatalf(".chain is at %s, chain.pin says %s: run ./fetch-chain.sh", strings.TrimSpace(string(got)), want)
	}
	mod, err := os.ReadFile(filepath.Join(chainDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	return mod
}

// protoDirs: the SDK's protos at the pinned chain's version, and the chain's.
func protoDirs(t testing.TB) []string {
	t.Helper()
	m := reRequire("github.com/cosmos/cosmos-sdk").FindSubmatch(chainGoMod(t))
	if m == nil {
		t.Fatal("chain go.mod names no cosmos-sdk version")
	}
	cache := os.Getenv("GOMODCACHE")
	if cache == "" {
		out, err := exec.Command("go", "env", "GOMODCACHE").Output()
		if err != nil {
			t.Fatal(err)
		}
		cache = strings.TrimSpace(string(out))
	}
	sdk := filepath.Join(cache, "github.com", "cosmos", "cosmos-sdk@"+string(m[1]), "proto")
	if _, err := os.Stat(sdk); err != nil {
		skipOutsideCI(t, "SDK protos not in the module cache ("+sdk+"): run ./fetch-chain.sh")
	}
	return []string{sdk, filepath.Join(chainDir, "proto")}
}

// skipOutsideCI: these tests need the pinned chain and the SDK's protos.
// Without them they skip, but in CI (CI set, as GitHub Actions does) a skip
// would pass silently with nothing checked, so it fails instead (R5-E-9).
func skipOutsideCI(t testing.TB, why string) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatal(why)
	}
	t.Skip(why)
}

// TestVersionsMatchChain: cometbft_test.go runs this module's CometBFT and
// gateway_test.go its grpc-gateway; both must be the versions the pinned
// chain links, or the tests prove agreement with code the node does not run.
func TestVersionsMatchChain(t *testing.T) {
	chain := chainGoMod(t)
	own, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, mod := range []string{"github.com/cometbft/cometbft", "github.com/grpc-ecosystem/grpc-gateway"} {
		if regexp.MustCompile(`(?m)^\s*(?:replace\s+)?` + regexp.QuoteMeta(mod) + `(\s+v\S+)?\s*=>`).Match(chain) {
			t.Errorf("the chain replaces %s; pin this module to the replacement by hand and adjust this test", mod)
			continue
		}
		c, o := reRequire(mod).FindSubmatch(chain), reRequire(mod).FindSubmatch(own)
		if c == nil || o == nil {
			t.Errorf("%s: chain %q, conformance %q", mod, c, o)
			continue
		}
		if string(c[1]) != string(o[1]) {
			t.Errorf("%s: the chain requires %s, conformance/go.mod %s", mod, c[1], o[1])
		}
	}
}
