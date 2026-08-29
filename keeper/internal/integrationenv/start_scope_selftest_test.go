package integrationenv

import (
	"go/parser"
	"go/token"
	"testing"
)

// The scope guard is the only thing standing between the NIM-569 convention and
// its 56th call site, and a guard that resolves nothing is indistinguishable
// from a tree that is clean. The first version of it derived an unaliased
// import's identifier from the last segment of the path, so `testcontainers-go`
// never matched the `testcontainers.` every generic call site writes: seven
// bring-ups were invisible and the suite was green. These cases are the
// difference between the two states.

func scan(t *testing.T, src string) (violations, blind []int) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return inspect(fset, file)
}

func TestGuardSeesAnUnaliasedRootImport(t *testing.T) {
	violations, blind := scan(t, `package x
import "github.com/testcontainers/testcontainers-go"
func f() { testcontainers.GenericContainer(nil, testcontainers.GenericContainerRequest{}) }
`)
	if len(violations) != 1 {
		t.Fatalf("got %d violation(s), want 1 -- an unaliased root import binds `testcontainers`, not the last path segment", len(violations))
	}
	if len(blind) != 0 {
		t.Fatalf("got %d blind, want 0", len(blind))
	}
}

func TestGuardAcceptsABringUpInsideStart(t *testing.T) {
	violations, _ := scan(t, `package x
import (
	"context"
	"github.com/testcontainers/testcontainers-go"
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
)
func f(ctx context.Context) {
	integrationenv.Start(ctx, "stand", func(ctx context.Context) (testcontainers.Container, error) {
		return testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{})
	})
}
`)
	if len(violations) != 0 {
		t.Fatalf("got %d violation(s), want 0: %v", len(violations), violations)
	}
}

func TestGuardResolvesEveryWayAModuleCanBeNamed(t *testing.T) {
	cases := map[string]string{
		"aliased as the tree does": `import tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
func f() { tcpostgres.Run(nil, "postgres:16-alpine") }`,
		"aliased to something else": `import pg "github.com/testcontainers/testcontainers-go/modules/postgres"
func f() { pg.Run(nil, "postgres:16-alpine") }`,
		"not aliased at all": `import "github.com/testcontainers/testcontainers-go/modules/postgres"
func f() { postgres.Run(nil, "postgres:16-alpine") }`,
		"a module nobody has used yet": `import "github.com/testcontainers/testcontainers-go/modules/redis"
func f() { redis.Run(nil, "redis:7") }`,
		"the deprecated spelling": `import "github.com/testcontainers/testcontainers-go/modules/vault"
func f() { vault.RunContainer(nil) }`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			violations, _ := scan(t, "package x\n"+body+"\n")
			if len(violations) != 1 {
				t.Fatalf("got %d violation(s), want 1", len(violations))
			}
		})
	}
}

func TestGuardIgnoresAnUnrelatedRun(t *testing.T) {
	violations, _ := scan(t, `package x
import "example.com/other/postgres"
func f() { postgres.Run(nil) }
`)
	if len(violations) != 0 {
		t.Fatalf("got %d violation(s), want 0", len(violations))
	}
}

func TestGuardReportsADotImportRatherThanReadingPastIt(t *testing.T) {
	violations, blind := scan(t, `package x
import . "github.com/testcontainers/testcontainers-go"
func f() { GenericContainer(nil, GenericContainerRequest{}) }
`)
	if len(blind) != 1 {
		t.Fatalf("got %d blind, want 1 -- a dot-import writes the bring-up as a bare identifier this guard cannot attribute", len(blind))
	}
	if len(violations) != 0 {
		t.Fatalf("got %d violation(s), want 0 (the file is reported as unreadable, not as clean)", len(violations))
	}
}

func TestAForeignStartDoesNotLaunderABringUp(t *testing.T) {
	violations, _ := scan(t, `package x
import "github.com/testcontainers/testcontainers-go"
func Start(f func()) {}
func f() {
	Start(func() { testcontainers.GenericContainer(nil, testcontainers.GenericContainerRequest{}) })
}
`)
	if len(violations) != 1 {
		t.Fatalf("got %d violation(s), want 1 -- only integrationenv.Start wraps, a same-named local function does not", len(violations))
	}
}

func TestIntegrationTagIsRecognisedAndNotOverReached(t *testing.T) {
	cases := map[string]struct {
		src  string
		want bool
	}{
		"the plain form the tree uses": {"//go:build integration\n\npackage x\n", true},
		"a compound constraint":        {"//go:build linux && integration\n\npackage x\n", true},
		"integration merely permitted": {"//go:build linux || integration\n\npackage x\n", false},
		"an unrelated tag":             {"//go:build linux\n\npackage x\n", false},
		"no constraint at all":         {"package x\n", false},
		"a constraint after package":   {"package x\n\n//go:build integration\n", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := hasIntegrationTag(tc.src); got != tc.want {
				t.Fatalf("hasIntegrationTag = %v, want %v", got, tc.want)
			}
		})
	}
}
