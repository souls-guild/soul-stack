package reaper

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// TestDispatch_SpawnDueCadence_NoLongerHandled — регресс C4 (ADR-048 §3): спавн
// Cadence переехал в Conductor, Reaper его больше НЕ исполняет. Правило
// `spawn_due_cadence` в keeper.yml теперь — неизвестное имя: dispatch обязан
// упасть в default-ветку с warn "unknown rule", а НЕ в специальную spawn-ветку.
//
// Доказательство «reaper больше не спавнит»: специальной ветки
// dispatchSpawnDueCadence нет, поля Deps.CadenceSpawn нет (закомпилировалось бы с
// ошибкой, если бы остались) — значит SELECT due-cadence из Reaper-тика исчез.
func TestDispatch_SpawnDueCadence_NoLongerHandled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := &Runner{deps: Deps{Logger: logger}}

	cfg := &config.KeeperConfig{Reaper: &config.KeeperReaper{
		Enabled: true,
		Rules:   map[string]config.ReaperRule{"spawn_due_cadence": {Enabled: true}},
	}}
	r.dispatch(context.Background(), cfg) // не должно паниковать

	out := buf.String()
	if !strings.Contains(out, "unknown rule") || !strings.Contains(out, "spawn_due_cadence") {
		t.Fatalf("ожидался warn 'unknown rule' для spawn_due_cadence (правило больше не в reaper); log=%q", out)
	}
}
