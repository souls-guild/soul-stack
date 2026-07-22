//go:build e2e

// L3b smoke-test of the Vigil/Decree/Oracle harness extension (ADR-030):
// exercises all 3 new helpers end-to-end through the real Operator API +
// real PG schema, WITHOUT a real mTLS-EventStream emit from the soul-stub.
//
// Covered:
//   - Stack.CreateVigil -- POST /v1/vigils -> 201 -> vigils row in DB.
//   - Stack.CreateDecree -- POST /v1/decrees -> 201 -> decrees row in DB.
//   - Stack.EmitPortent -- stub INSERT into oracle_fires (absence of a real
//     handlePortentEvent -- a documented simplification, see harness/oracle.go).
//   - Stack.WaitForOracleFires -- polls oracle_fires until N rows appear.
//
// NOT covered (and shouldn't be):
//   - full path Soul-stub -> mTLS-EventStream -> handlePortentEvent -> SubjectMatches
//     -> where-CEL -> EnqueueScenario -> RecordFire -> audit `oracle.fired`. That's
//     `TestE2EOracleTypedPortent_FileChangedFireFlow` (Skip until the full
//     harness extension with soul-stub-emit). L1 coverage is in
//     `keeper/internal/grpc/oracle_crosside_integration_test.go`.
package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestL3b_VigilDecreeOracleFlow_Smoke(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. CreateVigil -- core-beacon file_changed with a coven subject. coven=web
	// is arbitrary; the harness does not check host subject membership.
	vigilName := stack.CreateVigil(ctx, t, harness.CreateVigilOpts{
		Name:     "l3b-vigil-smoke",
		Interval: "30s",
		Check:    "core.beacon.file_changed",
		Coven:    []string{"web"},
		Params:   map[string]any{"path": "/etc/nginx.conf"},
	})

	// 2. CreateDecree -- reaction to this Vigil -> noop scenario. incarnation_name
	// is arbitrary (format validator only; the smoke test does not reach the
	// enqueue phase where membership is checked). where_cel is empty -- subject
	// binding is the only filter.
	decreeName := stack.CreateDecree(ctx, t, harness.CreateDecreeOpts{
		Name:            "l3b-decree-smoke",
		OnBeacon:        vigilName,
		Coven:           []string{"web"},
		IncarnationName: "web-app",
		ActionScenario:  "noop",
		Cooldown:        "5m",
	})

	// 3. EmitPortent -- stub fire into oracle_fires (see file header; the real
	// path via handlePortentEvent is a separate slice).
	subjectSID := "soul-test-0.example.com" // SID of the stub soul from NewStack (Souls:1).
	stack.EmitPortent(ctx, t, decreeName, subjectSID)

	// 4. WaitForOracleFires -- poll until 1 row (one unique (decree, subject)
	// pair).
	fires := stack.WaitForOracleFires(ctx, t, decreeName, 1, 5*time.Second)

	if len(fires) != 1 {
		t.Fatalf("expected 1 fire-row, got %d: %+v", len(fires), fires)
	}
	got := fires[0]
	if got.Decree != decreeName {
		t.Errorf("fires[0].Decree = %q, expected %q", got.Decree, decreeName)
	}
	if got.Subject != subjectSID {
		t.Errorf("fires[0].Subject = %q, expected %q", got.Subject, subjectSID)
	}
	if got.FiredAt.IsZero() {
		t.Error("fires[0].FiredAt is empty")
	}
}
