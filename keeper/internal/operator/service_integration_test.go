//go:build integration

// Integration-тесты Service.Revoke против реального Postgres: проверяют, что
// self-lockout-инвариант держится под конкурентными revoke-ами благодаря
// SELECT … FOR UPDATE (architect-verdict M0.6b §1, Slice 3). Unit-моки
// (service_test.go) сериализацию не доказывают — нужен настоящий row-level lock.
//
// Slice 3: lockout-probe берёт admin-set из БД (rbac.LockEffectiveClusterAdmins
// — JOIN rbac_role_operators × rbac_role_permissions × operators под
// FOR UPDATE OF ro,rp,o), НЕ из in-memory ClusterAdmins()-снимка. Поэтому
// seed создаёт реальную membership-строку (cluster-admin, <aid>), а не передаёт
// admin-set через fakeRBAC. Роль cluster-admin с permission `*` уже seed-нута
// миграцией 027.
//
// Issuer/RBAC здесь — fake (определены в service_test.go, общий пакет);
// fakeRBAC.admins больше НЕ участвует в lockout (БД-источник) — оставлен
// нулевым, чтобы доказать независимость инварианта от снимка.

package operator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// seedActiveOperator вставляет активного оператора. bootstrap=true → CreatedByAID
// nil (первый Архонт); иначе created_by_aid ссылается на parent.
func seedActiveOperator(t *testing.T, aid string, parent *string) {
	t.Helper()
	op := &Operator{
		AID:          aid,
		DisplayName:  aid,
		AuthMethod:   AuthMethodJWT,
		CreatedByAID: parent,
	}
	if err := Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seed %q: %v", aid, err)
	}
}

// seedClusterAdmin вставляет активного оператора И membership-строку
// (cluster-admin, aid) — делает его эффективным `*`-admin-ом в БД-источнике
// lockout-probe (Slice 3). Роль cluster-admin (+permission `*`) уже есть
// в схеме из миграции 027.
func seedClusterAdmin(t *testing.T, aid string, parent *string) {
	t.Helper()
	seedActiveOperator(t, aid, parent)
	grantClusterAdmin(t, aid)
}

// grantClusterAdmin добавляет membership (cluster-admin, aid). granted_by_aid
// = NULL (seed-membership без инициатора, как bootstrap).
func grantClusterAdmin(t *testing.T, aid string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO rbac_role_operators (role_name, aid, granted_by_aid)
		 VALUES ('cluster-admin', $1, NULL)`, aid)
	if err != nil {
		t.Fatalf("grant cluster-admin %q: %v", aid, err)
	}
}

func newIntegrationService(t *testing.T) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{
		Pool:   integrationPool,
		Issuer: &fakeIssuer{},
		// admins пуст намеренно: lockout-инвариант Slice 3 не должен от
		// ClusterAdmins()-снимка зависеть.
		RBAC:       &fakeRBAC{},
		TTLDefault: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func TestIntegration_ServiceRevoke_HappyPath(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)
	alice := "archon-alice"
	seedClusterAdmin(t, "archon-bob", &alice)

	s := newIntegrationService(t)
	err := s.Revoke(context.Background(), RevokeInput{
		AID: "archon-bob", Reason: "left team", CallerAID: "archon-alice",
	})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	got, err := SelectByAID(context.Background(), integrationPool, "archon-bob")
	if err != nil {
		t.Fatalf("SelectByAID: %v", err)
	}
	if !got.IsRevoked() {
		t.Errorf("archon-bob не revoked после Revoke")
	}
	if got.Metadata["revoke_reason"] != "left team" {
		t.Errorf("revoke_reason = %v, want \"left team\"", got.Metadata["revoke_reason"])
	}
}

// TestIntegration_ServiceRevoke_WouldLockOutCluster — единственный активный
// эффективный `*`-admin снимается → lockout (через БД-admin-set, не снимок).
func TestIntegration_ServiceRevoke_WouldLockOutCluster(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)

	s := newIntegrationService(t)
	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-alice", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster", err)
	}

	// Оператор остался активным — UPDATE откатился вместе с tx.
	got, err := SelectByAID(context.Background(), integrationPool, "archon-alice")
	if err != nil {
		t.Fatalf("SelectByAID: %v", err)
	}
	if got.IsRevoked() {
		t.Errorf("archon-alice revoked, want активен (lockout-инвариант)")
	}
}

// TestIntegration_ServiceRevoke_NotLastAdmin — revoke НЕ-последнего admin-а
// проходит: остаётся второй активный эффективный `*`-admin.
func TestIntegration_ServiceRevoke_NotLastAdmin(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)
	alice := "archon-alice"
	seedClusterAdmin(t, "archon-bob", &alice)

	s := newIntegrationService(t)
	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-alice", CallerAID: "archon-bob"})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, err := SelectByAID(context.Background(), integrationPool, "archon-alice")
	if err != nil {
		t.Fatalf("SelectByAID: %v", err)
	}
	if !got.IsRevoked() {
		t.Errorf("archon-alice не revoked после Revoke")
	}
}

// TestIntegration_ServiceRevoke_RevokedSecondAdminStillLocks — ЦЕНТРАЛЬНЫЙ
// кейс Slice 3 (закрывает пробел qa Slice 1). Второй cluster-admin уже
// ревокнут (revoked_at != NULL). Снимаем единственного активного → lockout:
// revoked НЕ считается «выжившим». Снимок ClusterAdmins() мог бы «помнить»
// второго как admin-а (staleness) и ошибочно пропустить revoke; БД-предикат
// `operators.revoked_at IS NULL` отсекает его жёстко.
func TestIntegration_ServiceRevoke_RevokedSecondAdminStillLocks(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)
	alice := "archon-alice"
	seedClusterAdmin(t, "archon-bob", &alice)

	s := newIntegrationService(t)
	ctx := context.Background()

	// Сначала легально снимаем bob (alice ещё активна — lockout не сработает).
	if err := s.Revoke(ctx, RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"}); err != nil {
		t.Fatalf("Revoke bob: %v", err)
	}
	bobGot, err := SelectByAID(ctx, integrationPool, "archon-bob")
	if err != nil {
		t.Fatalf("SelectByAID bob: %v", err)
	}
	if !bobGot.IsRevoked() {
		t.Fatalf("предусловие: archon-bob должен быть revoked")
	}

	// Теперь alice — единственный АКТИВНЫЙ эффективный `*`-admin (bob revoked,
	// но его membership-строка всё ещё есть). Снятие alice → lockout.
	err = s.Revoke(ctx, RevokeInput{AID: "archon-alice", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (revoked bob не должен считаться выжившим)", err)
	}

	got, err := SelectByAID(ctx, integrationPool, "archon-alice")
	if err != nil {
		t.Fatalf("SelectByAID alice: %v", err)
	}
	if got.IsRevoked() {
		t.Errorf("archon-alice revoked, want активен (lockout-инвариант)")
	}
}

func TestIntegration_ServiceRevoke_NotFound(t *testing.T) {
	resetOperators(t)
	s := newIntegrationService(t)
	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-ghost", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("err = %v, want ErrOperatorNotFound", err)
	}
}

func TestIntegration_ServiceRevoke_AlreadyRevoked(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)
	alice := "archon-alice"
	seedClusterAdmin(t, "archon-bob", &alice)

	s := newIntegrationService(t)
	ctx := context.Background()
	if err := s.Revoke(ctx, RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"}); err != nil {
		t.Fatalf("Revoke#1: %v", err)
	}
	err := s.Revoke(ctx, RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrOperatorAlreadyRevoked) {
		t.Fatalf("Revoke#2: err = %v, want ErrOperatorAlreadyRevoked", err)
	}
}

// TestIntegration_ServiceRevoke_ConcurrentLastAdmins — два активных
// cluster-admin-а, два параллельных Revoke (каждый ревокает другого). Без
// SELECT … FOR UPDATE оба могли бы пройти probe «admin-set ещё ≥ 2» и
// закоммитить → self-lockout. С сериализацией FOR UPDATE OF ro,rp,o ровно один
// преуспевает, второй видит, что остался последний активный admin, и получает
// ErrWouldLockOutCluster. Минимум один активный admin обязан остаться.
//
// Slice 3: admin-set приходит из БД, а не из снимка, поэтому гонка revoke ‖
// revoke (и revoke ‖ role-мутация — единое FOR UPDATE-ядро) сериализуется.
func TestIntegration_ServiceRevoke_ConcurrentLastAdmins(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)
	alice := "archon-alice"
	seedClusterAdmin(t, "archon-bob", &alice)

	s := newIntegrationService(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	targets := []RevokeInput{
		{AID: "archon-alice", CallerAID: "archon-bob"},
		{AID: "archon-bob", CallerAID: "archon-alice"},
	}
	for i := range targets {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			errs[idx] = s.Revoke(context.Background(), targets[idx])
		}(i)
	}
	close(start)
	wg.Wait()

	successes, lockouts := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			successes++
		case errors.Is(e, ErrWouldLockOutCluster):
			lockouts++
		default:
			t.Fatalf("неожиданная ошибка от Revoke: %v", e)
		}
	}
	if successes != 1 || lockouts != 1 {
		t.Fatalf("successes=%d lockouts=%d, want 1/1 (сериализация FOR UPDATE)", successes, lockouts)
	}

	// Инвариант: хотя бы один активный эффективный `*`-admin остался в БД.
	remaining := effectiveAdminCount(t)
	if remaining < 1 {
		t.Fatalf("активных admin-ов осталось %d, want >= 1 (кластер не должен залочиться)", remaining)
	}
}

// effectiveAdminCount — число активных операторов с эффективным `*` в БД.
// Считаем напрямую (read-only, без lock-а) для пост-проверки инварианта.
func effectiveAdminCount(t *testing.T) int {
	t.Helper()
	var n int
	err := integrationPool.QueryRow(context.Background(), `
		SELECT COUNT(DISTINCT ro.aid)
		FROM rbac_role_operators ro
		JOIN rbac_role_permissions rp ON rp.role_name = ro.role_name
		JOIN operators o ON o.aid = ro.aid
		WHERE rp.permission = '*' AND o.revoked_at IS NULL`).Scan(&n)
	if err != nil {
		t.Fatalf("effectiveAdminCount: %v", err)
	}
	return n
}
