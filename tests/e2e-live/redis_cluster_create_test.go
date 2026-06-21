//go:build e2e_live

// L3b E2E real-apply: ПОЛНЫЙ bootstrap-CREATE сервиса
// examples/service/redis-cluster с нуля на трёх ПОДЛИННЫХ soul-контейнерах.
//
// Это самый сложный staged-сценарий каталога: до сих пор create у redis-cluster
// был помечен «офлайн-неприменим» (cloud-spawn / declared-primary / probe на
// ещё-не-запущенном redis — см. redis_cluster_update_acl_test.go шапку, L3a
// redis_cluster_test.go). update_acl/remove_replica доказаны live (мутации
// поверх отдельно-поднятого redis), но НИКОГДА не доказывался полный жизненный
// цикл с нуля: install redis → declared-replication → ensure_users.
//
// Что закрывает этот тест (живой apply, не stub):
//   - (1) cloud-spawn `core.cloud.created` гейтован `when: has(input.spawn)`:
//     на pre-provisioned soul БЕЗ input.spawn шаг skipped, CloudDriver НЕ нужен —
//     create применим офлайн вопреки прежней пометке.
//   - (2) declared-роль `soulprint.hosts.where("role == 'primary'")[0]` из
//     incarnation.spec.hosts[].role. POST /v1/incarnations declared-hosts НЕ
//     принимает → seed-им spec.hosts[] direct SQL (SeedIncarnationForCreate)
//     ДО RunScenario(create). host-0=primary, host-1/2=replica.
//   - (3) vault redis_password через scoped vault:-ref (vault_scope secret/redis/*).
//   - (4) apply:destiny redis create-ветка: install.yml (core.pkg + redis.conf
//     render + service.running) → replication.yml (redis-replication-config +
//     health-gate retry.until) → ensure_users (core.cmd.shell ACL SETUSER на
//     declared-primary).
//
// ★ ASSERT на РЕАЛЬНОМ: redis установлен+запущен на всех 3 (redis-cli ping);
// replication настроена (host-0 master, host-1/2 slave + master_link_status:up
// через реальный redis-cli info replication); ACL-юзеры применены (redis-cli ACL
// GETUSER); incarnation.state консистентен; incarnation ready.
//
// ★★ AUTH-нюанс create-ветки: install.yml через redis.conf.tmpl ставит
// `requirepass <password>` на КАЖДОЙ ноде. Поэтому independent-verify redis-cli
// здесь ходит С `-a <password>` (в отличие от update_acl/split-brain тестов, где
// redis поднимался БЕЗ requirepass прямым Exec-ом). password известен тесту —
// это тот же литерал, что засеян в Vault.
package e2e_live_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// redisCreatePassword — пароль redis для create-прогона. Засевается в Vault
// (scoped vault:-ref `secret/redis/<inc>#password`), затем через requirepass
// попадает в redis.conf каждой ноды. min_length 16 (destiny redis input).
const redisCreatePassword = "create-redis-secret-32bytes"

func TestL3bRedisClusterCreate_FullLifecycle(t *testing.T) {
	t.Skip("БЛОКЕР (структурный, needs_architect): полный redis-cluster create использует host-вариативный flow-control в destiny (when: soulprint.self в redis-replication-config) на multi-host — движок отвергает (guardFlowControlHostInvariant, split-brain; per-host destiny-dispatch отложен ADR). Канон (orchestration §4.1) intends host-инвариантную destiny + per-роль where: на scenario-уровне; redis-cluster написан неправильным паттерном. Упрощённый redis-cluster-LIVE create доказан live (TestL3bRedisClusterLive_ThreeNode). Реактивировать после: (a) рерайт redis-cluster create на per-роль scenario-steps (where:primary/replica + host-инвариантные destiny) ЛИБО (b) per-host destiny-dispatch (engine feature, отложенный ADR).")

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: rcuExample,
		ServiceName: rcuService,
		Souls:       3,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 3 {
		t.Fatalf("ожидалось 3 soul-контейнера, получено %d", got)
	}
	const (
		primarySID  = "soul-live-a.example.com" // host-0 → declared primary
		replica1SID = "soul-live-b.example.com" // host-1 → declared replica
		replica2SID = "soul-live-c.example.com" // host-2 → declared replica
	)
	wantSIDs := []string{primarySID, replica1SID, replica2SID}
	for i, want := range wantSIDs {
		if got := stack.SoulContainers[i].SID; got != want {
			t.Fatalf("SoulContainers[%d].SID = %q, ожидалось %q", i, got, want)
		}
	}

	const incName = "redis-cluster-create"

	// ── (a) Coven-членство: roster резолвится по incarnation.name ∈ coven[]. ───
	for i := range stack.SoulContainers {
		stack.AddSoulToCoven(t, i, incName)
	}

	// ── (b) Ждём первый SoulprintReport на каждом хосте ────────────────────────
	// create читает soulprint.hosts (network.primary_ip) на render-фазе:
	// replication.yml резолвит master_addr = soulprint.hosts.where(primary)[0].
	// network.primary_ip, а redis.conf.tmpl биндит на soulprint.self.network.
	// primary_ip. Без фактов в БД render упадёт «no such key: primary_ip».
	for i := range stack.SoulContainers {
		stack.WaitSoulprintReported(t, i, 60)
	}

	// ── (c) Материализуем ОБЕ destiny-зависимости create-ветки ─────────────────
	// install.yml/ensure_users → destiny `redis` @ v2.0.0; replication.yml →
	// `redis-replication-config` @ v1.0.0 (service.yml::destiny[].ref).
	stack.MaterializeDestinies(t, "v2.0.0", "redis")
	stack.MaterializeDestinies(t, "v1.0.0", "redis-replication-config")

	// ── (d) Vault password: scoped vault:-ref secret/redis/<inc>#password ──────
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": redisCreatePassword})

	// ── (e) Seed incarnation с declared spec.hosts[role] ───────────────────────
	// ★ declared-роль засеяна direct SQL (POST не принимает declared-hosts):
	// host-0=primary, host-1/2=replica. status='ready' → RunScenario(create)
	// проходит lock-gate штатно. state пуст — create наполнит его сам.
	stack.SeedIncarnationForCreate(t, incName, rcuService, "main", []harness.SpecHostDecl{
		{SID: primarySID, Role: "primary"},
		{SID: replica1SID, Role: "replica"},
		{SID: replica2SID, Role: "replica"},
	})

	// ── (f) ★ RunScenario(create) БЕЗ input.spawn (cloud-шаг skipped) ──────────
	// redis_hosts — declared адреса (массив строк, state_changes пишет их в state).
	// redis_password — scoped vault:-ref. redis_users — один ACL-юзер `app`.
	applyID := stack.RunScenario(t, incName, "create", map[string]any{
		"redis_users": map[string]any{
			"app": map[string]any{"acl": "on >app-pass ~app:* +@all", "state": "on"},
		},
		"redis_config":   map[string]any{},
		"redis_hosts":    []any{primarySID, replica1SID, replica2SID},
		"redis_password": "vault:secret/redis/" + incName + "#password",
	})

	// staged create: install (Passage) → replication (Passage, health-gate
	// retry×12 delay 5s) → ensure_users. Реальный apt install redis + рестарт +
	// replication-sync на 3 контейнерах — широкий бюджет.
	stack.WaitApplySuccess(t, applyID, 600)
	stack.WaitIncarnationReady(t, incName, 60)

	masterIP := containerPrimaryIP(t, stack, 0)
	t.Logf("create: declared primary=%s master_ip=%s", primarySID, masterIP)

	// ── (1) redis установлен+запущен на ВСЕХ трёх (redis-cli -a PING) ──────────
	// requirepass из redis.conf.tmpl → ping без -a вернул бы NOAUTH; ходим с -a.
	for i := range stack.SoulContainers {
		assertRedisAuthPing(t, stack, i)
	}

	// ── (2) replication: host-0 master, host-1/2 slave + master_link_status:up ─
	// Реальный redis-cli info replication. role:master на host-0 доказывает, что
	// declared-primary НЕ получил replicaof (redis-replication-config no-op на
	// носителе master_addr). slave + link:up на репликах — что replicaof сошёлся.
	assertRedisRole(t, stack, 0, "master")
	assertRedisRole(t, stack, 1, "slave")
	assertRedisRole(t, stack, 2, "slave")
	assertReplicaLinkUp(t, stack, 1)
	assertReplicaLinkUp(t, stack, 2)

	// connected_slaves:2 на мастере — обе реплики подключены (cross-check к role).
	assertMasterSlaveCount(t, stack, 0, 2)

	// ── (3) ACL-юзеры применены на declared-primary (redis-cli ACL GETUSER) ────
	// ensure_users шёл широко по incarnation против master_addr (-h primary), на
	// репликах idempotent (та же команда тому же master). Проверяем на мастере.
	assertACLGetUserAuth(t, stack, 0, "app", []string{"~app:*", "+@all"})

	// ── (4) incarnation.state наполнен create-ветки state_changes ──────────────
	// redis_users из input; redis_hosts — declared адреса (массив строк, как
	// пишет create state_changes). redis_version "" (input.redis_version не задан).
	stack.AssertIncarnationState(t, incName, map[string]any{
		"redis_users": map[string]any{
			"app": map[string]any{"acl": "on >app-pass ~app:* +@all", "state": "on"},
		},
		"redis_hosts": []any{primarySID, replica1SID, replica2SID},
	})

	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "create",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// containerPrimaryIP возвращает primary IP i-го контейнера (hostname -i первый
// токен) — тот же адрес, что soul репортит в soulprint.self.network.primary_ip и
// что create использует как master_addr.
func containerPrimaryIP(t *testing.T, stack *harness.Stack, soulIdx int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := stack.SoulContainers[soulIdx].Exec(ctx,
		[]string{"sh", "-c", "hostname -i | awk '{print $1}'"})
	if err != nil || code != 0 {
		t.Fatalf("containerPrimaryIP[%d]: code=%d err=%v out=%s", soulIdx, code, err, out)
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		t.Fatalf("containerPrimaryIP[%d]: пустой IP (hostname -i = %q)", soulIdx, out)
	}
	return ip
}

// assertRedisAuthPing фейлит, если redis на i-м контейнере не отвечает PONG при
// аутентификации requirepass-паролем. Independent verify, что install-ветка
// create (core.pkg + redis.conf render + service.running) реально подняла redis.
func assertRedisAuthPing(t *testing.T, stack *harness.Stack, soulIdx int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := stack.SoulContainers[soulIdx].Exec(ctx,
		[]string{"redis-cli", "-a", redisCreatePassword, "ping"})
	if err != nil {
		t.Fatalf("assertRedisAuthPing[%d]: exec: %v", soulIdx, err)
	}
	if code != 0 || !strings.Contains(out, "PONG") {
		t.Fatalf("★ assertRedisAuthPing[%d]: redis НЕ ответил PONG (exit=%d out=%q) — install-ветка create не подняла redis с requirepass?",
			soulIdx, code, out)
	}
}

// assertRedisRole проверяет `redis-cli -a <pw> role` (первая строка) на i-м
// контейнере. wantRole — master | slave. Реальный verify топологии репликации.
func assertRedisRole(t *testing.T, stack *harness.Stack, soulIdx int, wantRole string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := stack.SoulContainers[soulIdx].Exec(ctx,
		[]string{"sh", "-c", "redis-cli -a " + redisCreatePassword + " role 2>/dev/null | head -1"})
	if err != nil {
		t.Fatalf("assertRedisRole[%d]: exec: %v", soulIdx, err)
	}
	if code != 0 {
		t.Fatalf("assertRedisRole[%d]: exit=%d out=%s", soulIdx, code, out)
	}
	got := strings.TrimSpace(out)
	if got != wantRole {
		t.Fatalf("★ assertRedisRole[%d]: role=%q, ожидалось %q (declared-replication create не сошёлся?)",
			soulIdx, got, wantRole)
	}
}

// assertReplicaLinkUp проверяет master_link_status:up на i-й реплике через
// `redis-cli -a <pw> info replication`. Поллинг — initial sync replicaof.
func assertReplicaLinkUp(t *testing.T, stack *harness.Stack, soulIdx int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, code, err := stack.SoulContainers[soulIdx].Exec(ctx,
			[]string{"sh", "-c", "redis-cli -a " + redisCreatePassword + " info replication 2>/dev/null"})
		if err == nil && code == 0 {
			last = out
			if strings.Contains(out, "master_link_status:up") {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("★ assertReplicaLinkUp[%d]: master_link_status:up не достигнут за 30s\nlast info:\n%s", soulIdx, last)
}

// assertMasterSlaveCount проверяет connected_slaves:<want> на мастере (i-й
// контейнер). Cross-check к role-ассертам: обе реплики реально подключены.
func assertMasterSlaveCount(t *testing.T, stack *harness.Stack, soulIdx, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deadline := time.Now().Add(30 * time.Second)
	wantLine := fmt.Sprintf("connected_slaves:%d", want)
	var last string
	for time.Now().Before(deadline) {
		out, code, err := stack.SoulContainers[soulIdx].Exec(ctx,
			[]string{"sh", "-c", "redis-cli -a " + redisCreatePassword + " info replication 2>/dev/null"})
		if err == nil && code == 0 {
			last = out
			if strings.Contains(out, wantLine) {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("★ assertMasterSlaveCount[%d]: %s не достигнут за 30s\nlast info:\n%s", soulIdx, wantLine, last)
}

// assertACLGetUserAuth выполняет `redis-cli -a <pw> ACL GETUSER <user>` на i-м
// контейнере и фейлит, если вывод не содержит каждую ожидаемую подстроку.
// Independent verify, что ensure_users реально применил ACL на живом redis.
func assertACLGetUserAuth(t *testing.T, stack *harness.Stack, soulIdx int, user string, wantSubstrings []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := stack.SoulContainers[soulIdx].Exec(ctx,
		[]string{"sh", "-c", "redis-cli -a " + redisCreatePassword + " ACL GETUSER " + user + " 2>/dev/null"})
	if err != nil {
		t.Fatalf("assertACLGetUserAuth[%d] %s: exec: %v", soulIdx, user, err)
	}
	if code != 0 {
		t.Fatalf("assertACLGetUserAuth[%d] %s: exit=%d out=%s", soulIdx, user, code, out)
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Fatalf("★ assertACLGetUserAuth[%d] %s: вывод не содержит %q (ensure_users не применил ACL?)\nACL GETUSER:\n%s",
				soulIdx, user, want, out)
		}
	}
}
