package reaper

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// TestDispatch_SpawnDueCadence_NoLongerHandled — regression C4 (ADR-048 §3): spawn
// Cadence moved to Conductor, Reaper no longer executes it. Rule
// `spawn_due_cadence` in keeper.yml is now an unknown name: dispatch must
// fall through to the default branch with warn "unknown rule", not to a special spawn branch.
//
// Proof "reaper no longer spawns": special dispatchSpawnDueCadence branch
// does not exist, Deps.CadenceSpawn field does not exist (would have compiled with
// error if it remained) — meaning SELECT due-cadence from Reaper tick disappeared.
func TestDispatch_SpawnDueCadence_NoLongerHandled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := &Runner{deps: Deps{Logger: logger}}

	cfg := &config.KeeperConfig{Reaper: &config.KeeperReaper{
		Enabled: true,
		Rules:   map[string]config.ReaperRule{"spawn_due_cadence": {Enabled: true}},
	}}
	r.dispatch(context.Background(), cfg) // should not panic

	out := buf.String()
	if !strings.Contains(out, "unknown rule") || !strings.Contains(out, "spawn_due_cadence") {
		t.Fatalf("expected warn 'unknown rule' for spawn_due_cadence (rule no longer in reaper); log=%q", out)
	}
}
