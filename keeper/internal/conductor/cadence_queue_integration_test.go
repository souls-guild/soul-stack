//go:build integration

package conductor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// TestIntegration_Spawn_HeldQueueRowsDoNotStarveTheBatch — NIM-1113. A `queue`
// schedule whose child waits for a window neither spawns nor advances, so it
// stays due for hours. One more of them than the batch holds, all older than an
// ordinary due schedule, used to fill every tick's `LIMIT` and keep the ordinary
// one from ever being picked. The due pick leaves them out, so the ordinary one
// spawns in the same tick, and a held row comes back as soon as its child ends.
func TestIntegration_Spawn_HeldQueueRowsDoNotStarveTheBatch(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	held := make([]string, defaultBatch+1)
	for i := range held {
		held[i] = fmt.Sprintf("01H%023d", i)
		seedCadence(t, held[i], cadence.OverlapPolicyQueue, -2*time.Hour)
		seedChild(t, held[i], voyage.StatusScheduled)
	}
	ordinary := seedCadence(t, cadenceID(60), cadence.OverlapPolicyParallel, -time.Minute)

	s := newSpawner(t, []string{"web-01"})
	if n, err := s.Run(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("tick: spawned %d, err %v; want 1 — the held queue rows took the batch", n, err)
	}
	if got := countVoyages(t, ordinary.ID); got != 1 {
		t.Fatalf("ordinary schedule's children = %d; want 1", got)
	}
	var moved int
	if err := integrationPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM cadences
		WHERE overlap_policy = 'queue' AND (last_run_at IS NOT NULL OR next_run_at > NOW())`).Scan(&moved); err != nil {
		t.Fatalf("count moved queue rows: %v", err)
	}
	if moved != 0 {
		t.Fatalf("%d held queue rows advanced; a held queue row must stay due", moved)
	}

	if _, err := integrationPool.Exec(ctx,
		`UPDATE voyages SET status = 'succeeded', finished_at = NOW() WHERE cadence_id = $1`, held[0]); err != nil {
		t.Fatalf("finish child: %v", err)
	}
	if n, err := s.Run(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("tick after the child ended: spawned %d, err %v; want 1", n, err)
	}
	if got := countVoyages(t, held[0]); got != 2 {
		t.Fatalf("released queue schedule's children = %d; want 2", got)
	}
}
