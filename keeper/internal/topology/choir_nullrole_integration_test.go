//go:build integration

// Integration-тесты NULL-role Voice (Choir, ADR-044) на real-PG. Закрывают
// поглощение declared-роли `voice.role > spec.hosts[].role` в вырожденном случае
// voice.role IS NULL: ключевой кейс ADR-044 п.2 (a) — опущенная роль = SQL NULL
// → fallback на spec.hosts[].role, а при отсутствии и spec-роли — пустая роль
// (хост вне declared-spec → soulprint.hosts[].role = null, ADR-008).
//
// До Wave5 Pass1 этот тест был невозможен из-за import-cycle (tide_target.go);
// после развязки резолвер topology тестируется на real-PG напрямую. Паттерн
// (testcontainers TestMain, resetAll/seed*-хелперы) совпадает с
// integration_test.go этого пакета.

package topology

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// seedChoir вставляет Choir (declared-«партию») в incarnation. Нужен из-за FK
// incarnation_choir_voices → incarnation_choirs(incarnation_name, choir_name).
func seedChoir(t *testing.T, incarnationName, choirName string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO incarnation_choirs (incarnation_name, choir_name) VALUES ($1, $2)`,
		incarnationName, choirName)
	if err != nil {
		t.Fatalf("seedChoir(%s/%s): %v", incarnationName, choirName, err)
	}
}

// seedVoiceNullRole вставляет Voice с role IS NULL (SQL NULL, не пустая строка) —
// эмулирует AddVoice с опущенной ролью (миграция 060: role TEXT без NOT NULL).
// Это ровно тот NULL, который резолвер сканит в *string и трактует как «нет
// роли» → fallback на spec.
func seedVoiceNullRole(t *testing.T, incarnationName, choirName, sid string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO incarnation_choir_voices (incarnation_name, choir_name, sid, role)
		 VALUES ($1, $2, $3, NULL)`,
		incarnationName, choirName, sid)
	if err != nil {
		t.Fatalf("seedVoiceNullRole(%s/%s/%s): %v", incarnationName, choirName, sid, err)
	}
}

// seedVoiceRole вставляет Voice с явной (не-NULL) role — контроль override-ветки:
// voice.role побеждает spec.hosts[].role (ADR-044 пункт 2). Параллель
// seedVoiceNullRole, но role биндится непустой строкой.
func seedVoiceRole(t *testing.T, incarnationName, choirName, sid, role string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO incarnation_choir_voices (incarnation_name, choir_name, sid, role)
		 VALUES ($1, $2, $3, $4)`,
		incarnationName, choirName, sid, role)
	if err != nil {
		t.Fatalf("seedVoiceRole(%s/%s/%s=%q): %v", incarnationName, choirName, sid, role, err)
	}
}

// assertNullRoleScannable — guard: NULL role в incarnation_choir_voices должна
// читаться через тот же *string-скан, что в резолвере (loadChoirMemberships).
// Если бы скан шёл в plain string, pgx падал бы «cannot scan NULL into *string»;
// этот SELECT ловит регрессию на уровне real-PG до основного assert-а теста.
func assertNullRoleScannable(t *testing.T, incarnationName, choirName, sid string) {
	t.Helper()
	var role *string
	err := integrationPool.QueryRow(context.Background(),
		`SELECT role FROM incarnation_choir_voices
		 WHERE incarnation_name = $1 AND choir_name = $2 AND sid = $3`,
		incarnationName, choirName, sid).Scan(&role)
	if err != nil {
		if err == pgx.ErrNoRows {
			t.Fatalf("Voice %s/%s/%s не вставлен", incarnationName, choirName, sid)
		}
		t.Fatalf("скан NULL role упал (регрессия cannot-scan-NULL): %v", err)
	}
	if role != nil {
		t.Fatalf("role = %q, ожидали SQL NULL (nil)", *role)
	}
}

// TestIntegration_NullVoiceRole_FallbacksToSpecRole — кейс (а) ADR-044 п.2(a):
// voice.role IS NULL, но spec.hosts[].role задан → итоговая role = spec-роль
// (NULL-голос НЕ затирает declared-роль, fallback). На real-PG проверяется и то,
// что NULL role не валит roster (скан в *string).
func TestIntegration_NullVoiceRole_FallbacksToSpecRole(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{
		"hosts": []map[string]any{
			{"sid": "a.example.com", "role": "replica"},
		},
	})
	seedSoul(t, "a.example.com", []string{"redis-prod"}, soul.StatusConnected)
	seedChoir(t, "redis-prod", "voters")
	seedVoiceNullRole(t, "redis-prod", "voters", "a.example.com")

	assertNullRoleScannable(t, "redis-prod", "voters", "a.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "a.example.com" {
		t.Fatalf("got %v, want [a.example.com]", sids(hosts))
	}
	if hosts[0].Role != "replica" {
		t.Errorf("role = %q, want spec-роль \"replica\" (NULL voice.role → fallback на spec)", hosts[0].Role)
	}
	// Voice без role всё равно даёт стабильный choir-факт для where:.
	if got := hosts[0].Choirs; len(got) != 1 || got[0] != "voters" {
		t.Errorf("Choirs = %v, want [voters] (членство есть даже при NULL role)", got)
	}
}

// TestIntegration_NullVoiceRole_NoSpec_RoleEmpty — кейс (б): voice.role IS NULL
// И spec.hosts[].role отсутствует (хост вне declared-spec) → итоговая role
// пустая (soulprint.hosts[].role = null, ADR-008), без паники/ошибки.
func TestIntegration_NullVoiceRole_NoSpec_RoleEmpty(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	// spec без hosts вовсе — хост точно вне declared-spec.
	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "b.example.com", []string{"redis-prod"}, soul.StatusConnected)
	seedChoir(t, "redis-prod", "voters")
	seedVoiceNullRole(t, "redis-prod", "voters", "b.example.com")

	assertNullRoleScannable(t, "redis-prod", "voters", "b.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "b.example.com" {
		t.Fatalf("got %v, want [b.example.com]", sids(hosts))
	}
	if hosts[0].Role != "" {
		t.Errorf("role = %q, want \"\" (NULL voice.role + нет spec → null, хост вне declared-spec)", hosts[0].Role)
	}
	if got := hosts[0].Choirs; len(got) != 1 || got[0] != "voters" {
		t.Errorf("Choirs = %v, want [voters]", got)
	}
}

// TestIntegration_ExplicitVoiceRole_OverridesSpecRole — кейс (в), контроль:
// voice.role задан явно (не NULL) И отличается от spec.hosts[].role → итоговая
// role = voice-роль (ADR-044 пункт 2 — Choir поглощает declared-роль). Проверяет,
// что NULL-ветка резолвера (nil *string → fallback) не сломала override: непустая
// role из колонки должна выигрывать у spec, а не теряться.
func TestIntegration_ExplicitVoiceRole_OverridesSpecRole(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	// spec даёт хосту роль "replica" — намеренно ДРУГУЮ, чем voice.role ниже.
	seedIncarnation(t, "redis-prod", map[string]any{
		"hosts": []map[string]any{
			{"sid": "c.example.com", "role": "replica"},
		},
	})
	seedSoul(t, "c.example.com", []string{"redis-prod"}, soul.StatusConnected)
	seedChoir(t, "redis-prod", "voters")
	seedVoiceRole(t, "redis-prod", "voters", "c.example.com", "primary")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "c.example.com" {
		t.Fatalf("got %v, want [c.example.com]", sids(hosts))
	}
	if hosts[0].Role != "primary" {
		t.Errorf("role = %q, want voice-роль \"primary\" (voice.role побеждает spec \"replica\")", hosts[0].Role)
	}
	if got := hosts[0].Choirs; len(got) != 1 || got[0] != "voters" {
		t.Errorf("Choirs = %v, want [voters]", got)
	}
}
