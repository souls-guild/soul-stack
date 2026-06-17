//go:build integration

// Integration-guard каскад-сноса постоянных notify-правил Cadence (ADR-052 §m /
// ADR-046 §9): DELETE cadence уносит Tiding-и с created_from_cadence_id == id
// (FK ON DELETE CASCADE, миграция 074), но НЕ трогает правила с тем же
// cadence-СЕЛЕКТОРОМ и created_from_cadence_id == NULL (вручную заведённые).
// Требует реального FK CASCADE → только под docker (testcontainers).

package herald

import (
	"context"
	"errors"
	"testing"
)

// insertCadence вставляет минимальную строку cadences напрямую (herald-пакет не
// импортирует cadence — для FK-цели достаточно raw INSERT обязательных колонок
// миграции 066: id/name/schedule_kind/overlap_policy/kind/target/created_by_aid).
func insertCadence(t *testing.T, id, aid string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO cadences (id, name, schedule_kind, interval_seconds, overlap_policy, kind, scenario_name, target, created_by_aid)
		 VALUES ($1, $2, 'interval', 300, 'skip', 'scenario', 'converge', '{"service":"web"}'::jsonb, $3)`,
		id, "nightly-"+id, aid)
	if err != nil {
		t.Fatalf("insertCadence(%s): %v", id, err)
	}
}

// cleanCadences чистит cadences (resetAll их не трогает напрямую; зависимые
// tidings уйдут каскадом за этим DELETE — что и есть предмет соседнего теста, но
// здесь — финальная уборка фикстуры).
func cleanCadences(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), `DELETE FROM cadences`); err != nil {
		t.Fatalf("cleanCadences: %v", err)
	}
}

func TestIntegration_Cadence_DeleteCascadesFormRules_NotManual(t *testing.T) {
	resetAll(t)
	cleanCadences(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	// Канал доставки (FK tidings.herald).
	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ops-webhook", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}

	// Расписание-цель каскада.
	const cadID = "01J0CADENCE000000000000001"
	insertCadence(t, cadID, "archon-alice")
	defer cleanCadences(t)

	// (1) Форм-правило: создано из notify[] формы Cadence — selector cadence +
	// origin-маркер created_from_cadence_id == cadID. Должно уйти каскадом.
	formRule := &Tiding{
		Name:                 "nightly-notify",
		Herald:               "ops-webhook",
		EventTypes:           []string{"scenario_run.failed"},
		Cadence:              strptr(cadID),
		CreatedFromCadenceID: strptr(cadID),
		Enabled:              true,
		CreatedByAID:         strptr("archon-alice"),
	}
	if err := InsertTiding(ctx, integrationPool, formRule); err != nil {
		t.Fatalf("InsertTiding form-rule: %v", err)
	}

	// (2) Вручную созданное правило с ТЕМ ЖЕ cadence-СЕЛЕКТОРОМ, но БЕЗ
	// origin-маркера (created_from_cadence_id == NULL). НЕ должно удаляться при
	// DELETE cadence — оператор завёл его руками, оно переживает снос расписания.
	manualRule := &Tiding{
		Name:         "manual-watch",
		Herald:       "ops-webhook",
		EventTypes:   []string{"scenario_run.completed"},
		Cadence:      strptr(cadID), // тот же селектор!
		Enabled:      true,
		CreatedByAID: strptr("archon-alice"),
	}
	if err := InsertTiding(ctx, integrationPool, manualRule); err != nil {
		t.Fatalf("InsertTiding manual-rule: %v", err)
	}

	// DELETE cadence → FK ON DELETE CASCADE сносит form-rule.
	if _, err := integrationPool.Exec(ctx, `DELETE FROM cadences WHERE id = $1`, cadID); err != nil {
		t.Fatalf("DELETE cadence: %v", err)
	}

	// Форм-правило удалено каскадом.
	if _, err := SelectTidingByName(ctx, integrationPool, "nightly-notify"); !errors.Is(err, ErrTidingNotFound) {
		t.Errorf("form-rule после DELETE cadence: err = %v, want ErrTidingNotFound (каскад)", err)
	}
	// Вручную созданное правило (created_from_cadence_id == NULL) ВЫЖИЛО, несмотря
	// на тот же cadence-селектор.
	survived, err := SelectTidingByName(ctx, integrationPool, "manual-watch")
	if err != nil {
		t.Fatalf("manual-rule после DELETE cadence удалено по ошибке: %v", err)
	}
	if survived.CreatedFromCadenceID != nil {
		t.Errorf("manual-rule.CreatedFromCadenceID = %v, want nil (вручную созданное)", survived.CreatedFromCadenceID)
	}
}

// Round-trip created_from_cadence_id через Insert/Select (ADR-052 §m): маркер
// корректно пишется и читается из колонки 074.
func TestIntegration_Tiding_CreatedFromCadenceRoundTrip(t *testing.T) {
	resetAll(t)
	cleanCadences(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ops-webhook", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}
	const cadID = "01J0CADENCE000000000000002"
	insertCadence(t, cadID, "archon-alice")
	defer cleanCadences(t)

	rule := &Tiding{
		Name:                 "nightly-notify",
		Herald:               "ops-webhook",
		EventTypes:           []string{"scenario_run.failed"},
		Cadence:              strptr(cadID),
		CreatedFromCadenceID: strptr(cadID),
		Enabled:              true,
		CreatedByAID:         strptr("archon-alice"),
	}
	if err := InsertTiding(ctx, integrationPool, rule); err != nil {
		t.Fatalf("InsertTiding: %v", err)
	}
	got, err := SelectTidingByName(ctx, integrationPool, "nightly-notify")
	if err != nil {
		t.Fatalf("SelectTidingByName: %v", err)
	}
	if got.CreatedFromCadenceID == nil || *got.CreatedFromCadenceID != cadID {
		t.Errorf("CreatedFromCadenceID round-trip = %v, want %q", got.CreatedFromCadenceID, cadID)
	}
}
