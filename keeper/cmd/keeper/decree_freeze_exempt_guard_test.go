package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// keeperModule is this module's import path; a package under it lives in the
// directory its path names below the module root.
const keeperModule = "github.com/souls-guild/soul-stack/keeper"

// freezeGates are the packages that hold a run back for a maintenance window
// or a freeze (ADR-0093): the calculator, the claim's start gate, the request's
// gate and consent check.
var freezeGates = map[string]string{
	keeperModule + "/internal/maintenance":  "the window and freeze calculator",
	keeperModule + "/internal/voyage":       "the Voyage claim's start gate (voyage.MaintenanceGate)",
	keeperModule + "/internal/api/handlers": "the request gate (handlers.PlanRunStart, handlers.CheckFreezeConsent)",
}

// TestDecreeReactionPathReachesNoFreezeGate guards the one exception of
// ADR-0093 §4 (NIM-1085): a Decree reaction is a repair and runs during a
// freeze. It reaches the work queue through oracle_enqueuer.go
// (applyrun.InsertPlanned), so neither that file nor anything it imports, at
// any depth, may reach a package that holds a run back — a gate there would
// hold the repair until the freeze ends. A dependency on one is a design
// change to bring to ADR-0093, not a fix for this test.
func TestDecreeReactionPathReachesNoFreezeGate(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	roots, err := fileImports(filepath.Join(root, "cmd", "keeper", "oracle_enqueuer.go"))
	if err != nil {
		t.Fatalf("parse oracle_enqueuer.go: %v", err)
	}

	via := map[string]string{} // package → the package that imported it
	queue := []string{}
	for _, p := range roots {
		if strings.HasPrefix(p, keeperModule+"/") {
			via[p] = "cmd/keeper/oracle_enqueuer.go"
			queue = append(queue, p)
		}
	}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		if why, ok := freezeGates[pkg]; ok {
			t.Errorf("the Decree reaction path reaches %s (%s): %s", pkg, why, chain(via, pkg))
			continue
		}
		deps, err := packageImports(filepath.Join(root, strings.TrimPrefix(pkg, keeperModule+"/")))
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, d := range deps {
			if !strings.HasPrefix(d, keeperModule+"/") {
				continue
			}
			if _, seen := via[d]; !seen {
				via[d] = pkg
				queue = append(queue, d)
			}
		}
	}
	if len(via) < 3 {
		t.Fatalf("walked %d packages from oracle_enqueuer.go, expected its applyrun/incarnation/grpc imports at least", len(via))
	}
}

func chain(via map[string]string, pkg string) string {
	parts := []string{pkg}
	for p := via[pkg]; p != ""; p = via[p] {
		parts = append(parts, p)
		if !strings.HasPrefix(p, keeperModule+"/") {
			break
		}
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, " -> ")
}

func fileImports(path string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(f.Imports))
	for _, s := range f.Imports {
		p, err := strconv.Unquote(s.Path.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// packageImports lists what the non-test files of the package in dir import.
// Build constraints are not evaluated: a file behind one still links in the
// configuration that builds it.
func packageImports(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		imps, err := fileImports(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, imps...)
	}
	return out, nil
}
