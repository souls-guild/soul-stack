//go:build integration

// Integration-тесты Apply через testcontainers-go.
//
// Поднимают postgres:16-alpine, гоняют Apply на чистой БД и проверяют
// идемпотентность + down/up cycle. Один контейнер per-package; между
// тестами state схемы дропается через resetSchema (DROP TABLE audit_log +
// DROP TABLE schema_migrations).
//
// Запуск:
//
//	make test-integration
//	# или
//	cd keeper && go test -tags=integration -race -count=1 ./internal/migrate/
package migrate_test

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	keepermigrate "github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var (
	integrationDSN  string
	integrationPool *pgxpool.Pool
)

// TestMain делегирует setup/teardown в run(), потому что os.Exit
// обходит defer-ы — context, контейнер и pool остались бы висеть.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run поднимает Postgres-контейнер, кладёт DSN/pool в package-vars,
// отдаёт m.Run(). Возвращает exit-code; defer-ы внутри функции корректно
// отрабатывают, потому что os.Exit вызывается уже в TestMain поверх
// возвращённого кода.
//
// SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true делает testcontainers
// обязательным (CI-режим): любой setup-fail → log.Fatalf. Без флага
// (локальный режим) — тесты skip-ятся при недоступном docker.
func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("migrate integration: setup failed (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER set): %v", err)
		}
		log.Printf("migrate integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("migrate integration: ConnectionString: %v", err)
		return 1
	}
	integrationDSN = dsn

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("migrate integration: pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

// resetSchema приводит БД в исходное состояние «чистая, без миграций».
// Тесты пакета шарят один контейнер (один per-package), поэтому между
// прогонами схема должна сбрасываться полностью.
//
// `DROP SCHEMA public CASCADE` + recreate сносит всё разом — все таблицы
// (audit_log/operators/incarnation/souls/apply_runs/providers/profiles/…),
// функции (purge_*/expire_*/mark_disconnected/…), типы, индексы и служебную
// schema_migrations golang-migrate-а — независимо от числа миграций. Это
// устойчиво к появлению новых миграций: не нужно перечислять объекты руками
// и держать список в синхроне с migrations/*.up.sql.
func resetSchema(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DROP SCHEMA public CASCADE`,
		`CREATE SCHEMA public`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("resetSchema %q: %v", stmt, err)
		}
	}
}

func TestIntegration_MigrateApply_FromScratch(t *testing.T) {
	resetSchema(t)
	ctx := context.Background()

	if err := keepermigrate.Apply(ctx, integrationDSN, migrations.FS, "."); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	assertAuditLogSchema(t, ctx)
	assertOperatorsSchema(t, ctx)
	assertAuditLogOperatorFK(t, ctx)
	assertRBACSchema(t, ctx)
}

// assertAuditLogSchema проверяет, что audit_log существует, три ожидаемых
// индекса присутствуют, и два из них (archon_aid, correlation_id) —
// partial (`WHERE … IS NOT NULL`). Вынесено для переиспользования между
// FromScratch и DownThenUp (gap-1/gap-3 из qa.A).
func assertAuditLogSchema(t *testing.T, ctx context.Context) {
	t.Helper()

	var exists bool
	err := integrationPool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_tables
			WHERE schemaname = current_schema() AND tablename = 'audit_log'
		)
	`).Scan(&exists)
	if err != nil {
		t.Fatalf("pg_tables: %v", err)
	}
	if !exists {
		t.Fatal("audit_log table missing")
	}

	// Три индекса из 001_create_audit_log.up.sql.
	wantIndexes := []string{
		"audit_log_event_type_created_at_idx",
		"audit_log_archon_aid_created_at_idx",
		"audit_log_correlation_id_idx",
	}
	for _, idx := range wantIndexes {
		var got bool
		err := integrationPool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_indexes
				WHERE schemaname = current_schema() AND indexname = $1
			)
		`, idx).Scan(&got)
		if err != nil {
			t.Fatalf("pg_indexes %q: %v", idx, err)
		}
		if !got {
			t.Errorf("index %q missing", idx)
		}
	}

	// Partial-индексы (`WHERE … IS NOT NULL`) — pg_index.indpred != NULL.
	// Если кто-то снимет `WHERE`-clause в up.sql, индекс станет full и
	// перестанет соответствовать схеме ADR-022.
	partialIndexes := []string{
		"audit_log_archon_aid_created_at_idx",
		"audit_log_correlation_id_idx",
	}
	for _, idx := range partialIndexes {
		var isPartial bool
		err := integrationPool.QueryRow(ctx, `
			SELECT i.indpred IS NOT NULL
			FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND c.relname = $1
		`, idx).Scan(&isPartial)
		if err != nil {
			t.Fatalf("pg_index %q: %v", idx, err)
		}
		if !isPartial {
			t.Errorf("index %q expected partial (WHERE ... IS NOT NULL), got full", idx)
		}
	}
}

func TestIntegration_MigrateApply_Idempotent(t *testing.T) {
	resetSchema(t)
	ctx := context.Background()

	if err := keepermigrate.Apply(ctx, integrationDSN, migrations.FS, "."); err != nil {
		t.Fatalf("Apply #1: %v", err)
	}
	// Второй вызов на той же БД — должен не вернуть ошибку
	// (golang-migrate в этом случае отдаёт ErrNoChange, Apply его глотает).
	if err := keepermigrate.Apply(ctx, integrationDSN, migrations.FS, "."); err != nil {
		t.Fatalf("Apply #2 (idempotent): %v", err)
	}
}

func TestIntegration_MigrateApply_DownThenUp(t *testing.T) {
	resetSchema(t)
	ctx := context.Background()

	if err := keepermigrate.Apply(ctx, integrationDSN, migrations.FS, "."); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Steps(-1) и Steps(1) не выносим в публичный Apply-API (down не
	// поддерживается из cmd/keeper, ADR-022 forward-only). Здесь вызываем
	// migrate напрямую, чтобы проверить, что .down.sql валиден и схема
	// действительно реконструируется обратно.
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	// Дублируем scheme-rewrite Apply (`postgres://` → `pgx5://`); тянуть
	// внутренний toMigrateURL не хочется — тест не должен зависеть от
	// внутренней реализации пакета.
	migrateURL := strings.Replace(integrationDSN, "postgres://", "pgx5://", 1)
	mm, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		t.Fatalf("migrate.NewWithSourceInstance: %v", err)
	}
	defer mm.Close()

	// Полный rollback всех применённых миграций (down). С появлением
	// миграций 003/004 (operators + FK) один-степовый откат больше не
	// дропает audit_log — он живёт в 001 и слетает только когда все
	// последующие миграции откачены. mm.Down() симметричен Apply (up to
	// latest) и не зависит от числа миграций.
	if err := mm.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Down: %v", err)
	}

	// После down — ни audit_log, ни operators, ни rbac_* не должны существовать.
	for _, tbl := range []string{
		"audit_log", "operators",
		"rbac_roles", "rbac_role_permissions", "rbac_role_operators",
	} {
		var exists bool
		err = integrationPool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_tables
				WHERE schemaname = current_schema() AND tablename = $1
			)
		`, tbl).Scan(&exists)
		if err != nil {
			t.Fatalf("pg_tables after down (%s): %v", tbl, err)
		}
		if exists {
			t.Fatalf("%s still exists after Down()", tbl)
		}
	}

	if err := mm.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Up: %v", err)
	}

	// После up — таблица и три индекса (включая partial-clause) должны
	// быть восстановлены полностью, симметрично FromScratch.
	assertAuditLogSchema(t, ctx)
	assertOperatorsSchema(t, ctx)
	assertAuditLogOperatorFK(t, ctx)
	assertRBACSchema(t, ctx)
}

// assertRBACSchema проверяет ADR-028 Фаза 1: три таблицы rbac_* существуют,
// seed-роль cluster-admin (builtin=true) с permission `*` присутствует
// (миграция 027 E1), и ON DELETE CASCADE с rbac_roles работает (удаление
// кастомной роли уносит её permissions и membership).
func assertRBACSchema(t *testing.T, ctx context.Context) {
	t.Helper()

	for _, tbl := range []string{"rbac_roles", "rbac_role_permissions", "rbac_role_operators"} {
		var exists bool
		err := integrationPool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_tables
				WHERE schemaname = current_schema() AND tablename = $1
			)
		`, tbl).Scan(&exists)
		if err != nil {
			t.Fatalf("pg_tables %s: %v", tbl, err)
		}
		if !exists {
			t.Fatalf("%s table missing", tbl)
		}
	}

	// Seed cluster-admin (E1): builtin=true + permission `*`.
	var builtin bool
	if err := integrationPool.QueryRow(ctx,
		`SELECT builtin FROM rbac_roles WHERE name = 'cluster-admin'`).Scan(&builtin); err != nil {
		t.Fatalf("seed cluster-admin missing: %v", err)
	}
	if !builtin {
		t.Error("cluster-admin builtin = false, want true (seed E1)")
	}
	var seedPerm string
	if err := integrationPool.QueryRow(ctx,
		`SELECT permission FROM rbac_role_permissions WHERE role_name = 'cluster-admin'`).Scan(&seedPerm); err != nil {
		t.Fatalf("seed cluster-admin permission missing: %v", err)
	}
	if seedPerm != "*" {
		t.Errorf("cluster-admin permission = %q, want *", seedPerm)
	}

	// CHECK rbac_roles_name_format: валидное kebab-case имя проходит,
	// невалидное (Uppercase) отвергается.
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name) VALUES ('cascade-probe')`); err != nil {
		t.Errorf("valid role name rejected: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name) VALUES ('BadName')`); err == nil {
		t.Error("invalid role name accepted, expected CHECK violation")
	}

	// ON DELETE CASCADE: permission кастомной роли уходит вместе с ролью.
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('cascade-probe', 'soul.list')`); err != nil {
		t.Fatalf("insert cascade-probe perm: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM rbac_roles WHERE name = 'cascade-probe'`); err != nil {
		t.Fatalf("delete cascade-probe role: %v", err)
	}
	var leftover int64
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM rbac_role_permissions WHERE role_name = 'cascade-probe'`).Scan(&leftover); err != nil {
		t.Fatalf("count leftover perms: %v", err)
	}
	if leftover != 0 {
		t.Errorf("ON DELETE CASCADE failed: %d permission rows survived role delete", leftover)
	}
}

// assertOperatorsSchema проверяет, что operators существует, partial
// unique index по `created_by_aid IS NULL` навешан (инвариант
// единственного bootstrap-Archon-а из ADR-013/014), и CHECK-constraints
// по AID-формату + auth_method enum валидируют ожидаемые значения.
func assertOperatorsSchema(t *testing.T, ctx context.Context) {
	t.Helper()

	var exists bool
	err := integrationPool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_tables
			WHERE schemaname = current_schema() AND tablename = 'operators'
		)
	`).Scan(&exists)
	if err != nil {
		t.Fatalf("pg_tables operators: %v", err)
	}
	if !exists {
		t.Fatal("operators table missing")
	}

	// Partial unique index — без него нарушится инвариант единственного
	// bootstrap-Archon-а (ADR-013).
	var isUniquePartial bool
	err = integrationPool.QueryRow(ctx, `
		SELECT i.indisunique AND i.indpred IS NOT NULL
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relname = 'operators_first_archon_idx'
	`).Scan(&isUniquePartial)
	if err != nil {
		t.Fatalf("pg_index operators_first_archon_idx: %v", err)
	}
	if !isUniquePartial {
		t.Error("operators_first_archon_idx должен быть UNIQUE + partial (WHERE created_by_aid IS NULL)")
	}

	// CHECK aid_format: валидный AID проходит, невалидный отвергается.
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('archon-test-ok', 'OK', 'jwt')
	`); err != nil {
		t.Errorf("valid AID rejected: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('NOT-AN-AID', 'Bad', 'jwt')
	`); err == nil {
		t.Error("invalid AID accepted, expected CHECK violation")
	}
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('archon-test-bad-auth', 'Bad', 'sso')
	`); err == nil {
		t.Error("invalid auth_method accepted, expected CHECK violation")
	}

	// Инвариант partial unique: второй INSERT с created_by_aid IS NULL —
	// нарушение (первый archon-test-ok уже занял этот «слот»).
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('archon-second-bootstrap', 'Bootstrap?', 'jwt')
	`); err == nil {
		t.Error("второй operator с created_by_aid=NULL принят, ожидали unique violation")
	}

	// Cleanup, чтобы не мешать DownThenUp / последующим TestIntegration_*.
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM operators WHERE aid = 'archon-test-ok'`,
	); err != nil {
		t.Fatalf("cleanup operators: %v", err)
	}
}

// assertAuditLogOperatorFK проверяет, что миграция 004 действительно
// создала FK `audit_log.archon_aid → operators(aid)` с ON DELETE SET NULL.
func assertAuditLogOperatorFK(t *testing.T, ctx context.Context) {
	t.Helper()

	// confdeltype — PostgreSQL `"char"` (1 byte, OID 18); pgx в бинарном
	// протоколе не маппит его в обычные Go-типы. Кастуем в text прямо в
	// SQL — тривиально и читаемо.
	var deleteAction string
	err := integrationPool.QueryRow(ctx, `
		SELECT confdeltype::text
		FROM pg_constraint
		WHERE conname = 'audit_log_archon_aid_fk'
	`).Scan(&deleteAction)
	if err != nil {
		t.Fatalf("pg_constraint audit_log_archon_aid_fk: %v", err)
	}
	// 'n' = SET NULL в pg_constraint.confdeltype.
	if deleteAction != "n" {
		t.Errorf("FK ON DELETE = %q (pg_constraint.confdeltype), want 'n' (SET NULL)", deleteAction)
	}
}

// TestIntegration_MigrateApply_DirtyState проверяет, что после неуспешной
// миграции (битый SQL) golang-migrate помечает версию dirty, и повторный
// up вернёт migrate.ErrDirty. Это защищает keeper-startup от тихого
// ре-старта на полу-применённой схеме.
//
// Apply (продовый) принимает `embed.FS` — подменить его in-memory нельзя
// без `//go:embed`-конкретики. Поэтому тест использует golang-migrate
// напрямую с in-memory source (httpfs), симулируя то, что произошло бы
// внутри Apply при битой up.sql. Покрытие достаточное: dirty-state живёт
// в БД (schema_migrations.dirty), а не в Go-логике Apply.
func TestIntegration_MigrateApply_DirtyState(t *testing.T) {
	resetSchema(t)

	// Битая миграция: CREATE TABLE, потом синтаксически некорректный SQL.
	// pgx5 driver выполняет up в транзакции; SQL-ошибка обнуляет changes,
	// но schema_migrations.dirty при этом всё равно ставится — это
	// инвариант golang-migrate-а независимо от атомарности самого DDL.
	brokenFS := fstest.MapFS{
		"001_dirty.up.sql":   {Data: []byte("CREATE TABLE dirty_test (id INT); SELECT broken syntax here;")},
		"001_dirty.down.sql": {Data: []byte("DROP TABLE IF EXISTS dirty_test;")},
	}
	migrateURL := strings.Replace(integrationDSN, "postgres://", "pgx5://", 1)

	src1, err := iofs.New(brokenFS, ".")
	if err != nil {
		t.Fatalf("iofs.New #1: %v", err)
	}
	mm1, err := migrate.NewWithSourceInstance("inmem", src1, migrateURL)
	if err != nil {
		t.Fatalf("migrate.NewWithSourceInstance #1: %v", err)
	}
	if err := mm1.Up(); err == nil {
		mm1.Close()
		t.Fatal("Up on broken migration: ожидали ошибку, получили nil")
	}
	mm1.Close()

	// Повторный Up — должен увидеть dirty=true и вернуть migrate.ErrDirty.
	// New driver instance + новый source: предыдущий mm1.Close() закрыл
	// и source, и database connection.
	src2, err := iofs.New(brokenFS, ".")
	if err != nil {
		t.Fatalf("iofs.New #2: %v", err)
	}
	mm2, err := migrate.NewWithSourceInstance("inmem", src2, migrateURL)
	if err != nil {
		t.Fatalf("migrate.NewWithSourceInstance #2: %v", err)
	}
	defer mm2.Close()
	err = mm2.Up()
	if err == nil {
		t.Fatal("Up on dirty state: ожидали ошибку, получили nil")
	}
	var dirtyErr migrate.ErrDirty
	if !errors.As(err, &dirtyErr) {
		t.Fatalf("Up on dirty state: ожидали migrate.ErrDirty, получили %T: %v", err, err)
	}

	// schema_migrations с dirty=true и custom-таблицей dirty_test (если
	// частично создалась) останутся между тестами; следующий resetSchema
	// в начале другого Test* уберёт audit_log + schema_migrations, но не
	// dirty_test — поэтому чистим тут руками.
	cleanCtx := context.Background()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS dirty_test`,
		`DROP TABLE IF EXISTS schema_migrations`,
	} {
		if _, err := integrationPool.Exec(cleanCtx, stmt); err != nil {
			t.Fatalf("dirty cleanup %q: %v", stmt, err)
		}
	}
}

// TestIntegration_MigrateApply_CtxCanceled моделирует SIGTERM, прилетевший в
// момент старта Apply (pre-cancelled ctx). Прерывание миграций через
// GracefulStop — best-effort, а не гарантия: ctx-bridge-горутина конкурирует с
// m.Up(), и на свежей схеме миграции буферизуются быстрее, чем bridge успевает
// послать stop. Поэтому оба исхода допустимы: Apply либо прервался (err != nil),
// либо успел домигрировать (err == nil). Что тест ОБЯЗАН гарантировать — pre-cancel
// не оставляет схему в битом/dirty состоянии: повторный Apply на чистом ctx
// должен пройти идемпотентно.
func TestIntegration_MigrateApply_CtxCanceled(t *testing.T) {
	resetSchema(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // отменяем до запуска

	// Не паникует; ошибка прерывания допустима, но не требуется (best-effort).
	_ = keepermigrate.Apply(ctx, integrationDSN, migrations.FS, ".")

	// Состояние согласовано: повторный Apply на живом ctx доводит схему до
	// конца без dirty-ошибки — pre-cancel не порвал миграцию на полпути.
	if err := keepermigrate.Apply(context.Background(), integrationDSN, migrations.FS, "."); err != nil {
		t.Fatalf("повторный Apply после pre-cancel: %v (схема осталась в несогласованном состоянии)", err)
	}
}
