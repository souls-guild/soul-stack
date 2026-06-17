//go:build integration

// Integration-тесты RBAC-repository (ADR-028 Фаза 1) через testcontainers-go.
//
// Поднимают postgres:16-alpine, применяют миграции 026/027, проверяют
// LoadSnapshot / GrantOperator против реальной БД. Один контейнер per-package.
//
// Запуск:
//
//	cd keeper && go test -tags=integration -race -count=1 ./internal/rbac/

package rbac

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

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
			log.Fatalf("rbac integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("rbac integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("rbac integration: ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("rbac integration: migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("rbac integration: pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

// resetRBAC приводит RBAC-таблицы в исходное состояние «только seed-роль
// cluster-admin». TRUNCATE operators CASCADE каскадно truncate-ит и rbac_roles
// (FK created_by_aid → operators) вместе с её permissions/membership — поэтому
// после wipe ре-сидим cluster-admin идемпотентно (в проде её держит миграция
// 027; здесь воспроизводим post-wipe-состояние явно).
func resetRBAC(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`TRUNCATE TABLE operators, audit_log CASCADE`); err != nil {
		t.Fatalf("resetRBAC TRUNCATE: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO rbac_roles (name, builtin, created_by_aid)
		 VALUES ('cluster-admin', true, NULL) ON CONFLICT (name) DO NOTHING`,
		`INSERT INTO rbac_role_permissions (role_name, permission)
		 VALUES ('cluster-admin', '*') ON CONFLICT (role_name, permission) DO NOTHING`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("resetRBAC re-seed: %v", err)
		}
	}
}

// seedOperator вставляет оператора для удовлетворения FK rbac_role_operators.aid.
//
// bootstrap=true → created_by_aid IS NULL (первый Архонт; ровно один разрешён
// partial unique index-ом). Последующие операторы вставляются с
// createdBy=<bootstrap-aid>, иначе нарушат operators_first_archon_idx.
func seedOperator(t *testing.T, aid string, createdBy *string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`INSERT INTO operators (aid, display_name, auth_method, created_by_aid)
		 VALUES ($1, $1, 'jwt', $2)`, aid, createdBy,
	); err != nil {
		t.Fatalf("seed operator %s: %v", aid, err)
	}
}

func TestIntegration_LoadSnapshot_SeedOnly(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()

	snap, err := LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	perms, ok := snap.Roles["cluster-admin"]
	if !ok {
		t.Fatalf("seed cluster-admin missing: %+v", snap.Roles)
	}
	if len(perms) != 1 || perms[0] != "*" {
		t.Errorf("cluster-admin perms = %v, want [*]", perms)
	}
	// Без membership-а — пустой Membership.
	if len(snap.Membership) != 0 {
		t.Errorf("Membership = %v, want empty (no grants yet)", snap.Membership)
	}
}

func TestIntegration_GrantOperator_Idempotent(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)

	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("GrantOperator #1: %v", err)
	}
	// Повторный grant той же пары — no-op (ON CONFLICT DO NOTHING).
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("GrantOperator #2 (idempotent): %v", err)
	}

	var n int64
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM rbac_role_operators WHERE aid = 'archon-alice'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("membership rows = %d, want 1 (idempotent)", n)
	}

	// granted_by_aid IS NULL у bootstrap-membership-а.
	var grantedBy *string
	if err := integrationPool.QueryRow(ctx,
		`SELECT granted_by_aid FROM rbac_role_operators WHERE aid = 'archon-alice'`).Scan(&grantedBy); err != nil {
		t.Fatalf("scan granted_by_aid: %v", err)
	}
	if grantedBy != nil {
		t.Errorf("granted_by_aid = %v, want NULL", *grantedBy)
	}
}

func TestIntegration_GrantOperator_GrantedByAID(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	seedOperator(t, "archon-bob", &alice)

	by := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-bob", &by); err != nil {
		t.Fatalf("GrantOperator with granted_by: %v", err)
	}
	var grantedBy *string
	if err := integrationPool.QueryRow(ctx,
		`SELECT granted_by_aid FROM rbac_role_operators WHERE aid = 'archon-bob'`).Scan(&grantedBy); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if grantedBy == nil || *grantedBy != by {
		t.Errorf("granted_by_aid = %v, want %q", grantedBy, by)
	}
}

func TestIntegration_GrantOperator_FKViolation_UnknownRole(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)

	err := GrantOperator(ctx, integrationPool, "ghost-role", "archon-alice", nil)
	if err == nil {
		t.Fatal("GrantOperator on unknown role: expected FK violation, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("err = %v, want FK violation (23503)", err)
	}
}

func TestIntegration_GrantOperator_FKViolation_UnknownAID(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()

	err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-ghost", nil)
	if err == nil {
		t.Fatal("GrantOperator on unknown AID: expected FK violation, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("err = %v, want FK violation (23503)", err)
	}
}

// TestIntegration_LoadSnapshot_IncludesRevoked — ADR-014 Amendment 2026-05-27:
// после UPDATE operators.revoked_at LoadSnapshot отдаёт ревокнутого AID в
// Snapshot.Revoked; активные операторы там отсутствуют. Полный путь
// «revoke → enforcer.Check вернёт ErrOperatorRevoked».
func TestIntegration_LoadSnapshot_IncludesRevoked(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-active", nil)
	active := "archon-active"
	seedOperator(t, "archon-fired", &active)

	if _, err := integrationPool.Exec(ctx,
		`UPDATE operators SET revoked_at = NOW() WHERE aid = 'archon-fired'`); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	snap, err := LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if _, ok := snap.Revoked["archon-fired"]; !ok {
		t.Errorf("Revoked[archon-fired] отсутствует: %+v", snap.Revoked)
	}
	if _, ok := snap.Revoked["archon-active"]; ok {
		t.Errorf("Revoked[archon-active] = true, want false (активный оператор не в выборке)")
	}

	// Сквозной путь: enforcer построен из снимка → Check для revoked AID
	// возвращает ErrOperatorRevoked.
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-fired", &active); err != nil {
		t.Fatalf("GrantOperator: %v", err)
	}
	snap, err = LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot после grant: %v", err)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-fired", "operator", "create", nil); !errors.Is(err, ErrOperatorRevoked) {
		t.Errorf("Check(archon-fired): %v, want ErrOperatorRevoked", err)
	}
}

// TestIntegration_Snapshot_To_Enforcer — full path: grant двух ролей одному
// AID, LoadSnapshot → NewEnforcerFromSnapshot → Check резолвит union permissions.
func TestIntegration_Snapshot_To_Enforcer(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-x", nil)

	// Кастомная роль soul-reader через прямой INSERT (role.create — Фаза 2).
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name, builtin) VALUES ('soul-reader', false)`); err != nil {
		t.Fatalf("insert role: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('soul-reader', 'soul.list')`); err != nil {
		t.Fatalf("insert perm: %v", err)
	}
	if err := GrantOperator(ctx, integrationPool, "soul-reader", "archon-x", nil); err != nil {
		t.Fatalf("grant soul-reader: %v", err)
	}

	snap, err := LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-x", "soul", "list", nil); err != nil {
		t.Errorf("archon-x should pass soul.list: %v", err)
	}
	if err := e.Check("archon-x", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("archon-x should be denied operator.create: %v", err)
	}
}

// TestIntegration_SynodMembershipUnion — ADR-049 на реальной PG: роль через
// Synod даёт permission так же, как прямой грант (Check). seedSynod + добавление
// архона в группу (synod_operators) + bundle роли в группу (synod_roles) →
// LoadSnapshot разворачивает union → enforcer.Check проходит.
func TestIntegration_SynodMembershipUnion(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-x", nil)

	for _, stmt := range []string{
		`INSERT INTO rbac_roles (name, builtin) VALUES ('prod-ops', false)`,
		`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('prod-ops', 'incarnation.run')`,
		`INSERT INTO synods (name, builtin) VALUES ('team-prod', false)`,
		`INSERT INTO synod_roles (synod_name, role_name) VALUES ('team-prod', 'prod-ops')`,
		`INSERT INTO synod_operators (synod_name, aid) VALUES ('team-prod', 'archon-x')`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed Synod (%s): %v", stmt, err)
		}
	}

	snap, err := LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if got := snap.Membership["archon-x"]; len(got) != 1 || got[0] != "prod-ops" {
		t.Fatalf("Membership[archon-x] = %v, want [prod-ops] (через Synod)", got)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-x", "incarnation", "run", nil); err != nil {
		t.Errorf("archon-x should pass incarnation.run via Synod: %v", err)
	}
}

// TestIntegration_SynodCascadeOnDelete — ADR-049(d): DELETE synods каскадно
// чистит synod_operators и synod_roles (FK ON DELETE CASCADE с обеих сторон).
func TestIntegration_SynodCascadeOnDelete(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-x", nil)

	for _, stmt := range []string{
		`INSERT INTO rbac_roles (name, builtin) VALUES ('prod-ops', false)`,
		`INSERT INTO synods (name, builtin) VALUES ('team-prod', false)`,
		`INSERT INTO synod_roles (synod_name, role_name) VALUES ('team-prod', 'prod-ops')`,
		`INSERT INTO synod_operators (synod_name, aid) VALUES ('team-prod', 'archon-x')`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if _, err := integrationPool.Exec(ctx, `DELETE FROM synods WHERE name = 'team-prod'`); err != nil {
		t.Fatalf("DELETE synods: %v", err)
	}

	for _, tbl := range []string{"synod_operators", "synod_roles"} {
		var n int64
		if err := integrationPool.QueryRow(ctx,
			`SELECT COUNT(*) FROM `+tbl+` WHERE synod_name = 'team-prod'`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("%s rows = %d after DELETE synods, want 0 (ON DELETE CASCADE)", tbl, n)
		}
	}
}

// TestIntegration_SynodRoleCascadeOnRoleDelete — ADR-049(d): DELETE rbac_roles
// снимает роль из всех Synod-bundle-ов (synod_roles FK на rbac_roles ON DELETE
// CASCADE), не трогая саму группу и её membership.
func TestIntegration_SynodRoleCascadeOnRoleDelete(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-x", nil)

	for _, stmt := range []string{
		`INSERT INTO rbac_roles (name, builtin) VALUES ('prod-ops', false)`,
		`INSERT INTO synods (name, builtin) VALUES ('team-prod', false)`,
		`INSERT INTO synod_roles (synod_name, role_name) VALUES ('team-prod', 'prod-ops')`,
		`INSERT INTO synod_operators (synod_name, aid) VALUES ('team-prod', 'archon-x')`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if _, err := integrationPool.Exec(ctx, `DELETE FROM rbac_roles WHERE name = 'prod-ops'`); err != nil {
		t.Fatalf("DELETE rbac_roles: %v", err)
	}

	var roleRows int64
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM synod_roles WHERE role_name = 'prod-ops'`).Scan(&roleRows); err != nil {
		t.Fatalf("count synod_roles: %v", err)
	}
	if roleRows != 0 {
		t.Errorf("synod_roles rows = %d after DELETE role, want 0 (ON DELETE CASCADE)", roleRows)
	}
	// Группа и её membership остаются (роль ушла — bundle опустел).
	var opRows int64
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM synod_operators WHERE synod_name = 'team-prod'`).Scan(&opRows); err != nil {
		t.Fatalf("count synod_operators: %v", err)
	}
	if opRows != 1 {
		t.Errorf("synod_operators rows = %d, want 1 (membership не трогается DELETE-ом роли)", opRows)
	}
}
