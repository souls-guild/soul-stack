// Package rbactest — тест-фикстуры RBAC для пакетов keeper (api / mcp /
// middleware / handlers / rbac). После hard-cut-а config-RBAC (ADR-028(g))
// единственный источник enforcer-а — БД-снимок [rbac.Snapshot]; тесты,
// которым раньше хватало `config.KeeperRBAC`, собирают эквивалентный снимок
// вручную через этот хелпер.
//
// Форма [Config]/[Role] повторяет прежний config-RBAC (default_policy +
// roles[].{name,operators,permissions}) — те же permission-кейсы, только
// источник = Snapshot вместо keeper.yml. Живёт в обычном (не `_test.go`)
// файле, потому что фикстура шарится между несколькими тест-пакетами.
package rbactest

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// Config — тест-форма RBAC-каталога: плоский список ролей с привязками.
// Аналог удалённого `config.KeeperRBAC`; `DefaultPolicy` сохранено для
// читаемости фикстур (deny — единственный режим MVP), на сборку Snapshot
// не влияет.
//
// Revoked — фикстура для ADR-014 Amendment 2026-05-27 (JWT immediate revoke):
// AID → revoked_at. Не пересекается с Roles/Operators — оператор может быть
// одновременно в Membership и в Revoked (моделирует «уволенного, но ещё с
// активной ролью в каталоге»).
type Config struct {
	DefaultPolicy string
	Roles         []Role
	Revoked       map[string]time.Time
}

// Role — одна роль фикстуры: имя, привязанные AID-ы, permission-строки (RAW).
type Role struct {
	Name        string
	Operators   []string
	Permissions []string
}

// Snapshot собирает [rbac.Snapshot] из тест-конфига: роль → permissions
// (Roles) и AID → роли (Membership). nil-конфиг → пустой снимок (default
// deny), как nil-снимок в [rbac.NewEnforcerFromSnapshot].
func Snapshot(cfg *Config) *rbac.Snapshot {
	if cfg == nil {
		return nil
	}
	snap := &rbac.Snapshot{
		Roles:      make(map[string][]string, len(cfg.Roles)),
		Membership: make(map[string][]string),
		Revoked:    make(map[string]time.Time, len(cfg.Revoked)),
	}
	for _, r := range cfg.Roles {
		snap.Roles[r.Name] = r.Permissions
		for _, aid := range r.Operators {
			snap.Membership[aid] = append(snap.Membership[aid], r.Name)
		}
	}
	for aid, at := range cfg.Revoked {
		snap.Revoked[aid] = at
	}
	return snap
}

// NewEnforcer строит [rbac.Enforcer] из тест-конфига через БД-снимок-путь.
// Возвращает ошибку парсинга permission-строк (тот же [rbac.ParsePermission],
// что и прод-путь) — тесты на «unknown permission → fatal» этим пользуются.
func NewEnforcer(cfg *Config) (*rbac.Enforcer, error) {
	return rbac.NewEnforcerFromSnapshot(Snapshot(cfg))
}

// MustEnforcer — NewEnforcer с t.Fatalf на ошибке. Удобно для большинства
// фикстур, где невалидный permission — это баг теста, а не проверяемый кейс.
func MustEnforcer(t *testing.T, cfg *Config) *rbac.Enforcer {
	t.Helper()
	e, err := NewEnforcer(cfg)
	if err != nil {
		t.Fatalf("rbactest.NewEnforcer: %v", err)
	}
	return e
}
