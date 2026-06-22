//go:build e2e_live

// L3b E2E pilot: examples/service/redis::create (ADR-039) — real-soul-in-container.
//
// Параллель с tests/e2e/redis_test.go (L3a, soul-stub отвечает scripted), но
// идущая через РЕАЛЬНЫЙ apt-install redis-server + GitHub-tarball-установку
// redis_exporter/node_exporter внутри Debian-12-systemd-soul-container. Первый
// L3b-сервис-композиция через apply:destiny (ADR-009): create раскрывает три
// standalone-destiny (redis-single → redis-exporter → node-exporter).
//
// Покрытие, которого L3a не даёт: Keeper render → ApplyRequest на wire → реальный
// soul Apply (core.pkg / core.url / core.archive / core.cmd / core.file.rendered /
// core.service) → RunResult → apply_runs success. ★ Центральная цель — piggyback-
// проверка node-exporter: бинарь разложен, systemd-unit активен И порт :9100
// реально слушает /metrics (см. expectations/after-create.yaml::host_state.endpoints).
//
// Harness-механики, вскрытые этим pilot-ом (см. tests/e2e-live/harness):
//   - SeedVaultKV: redis-пароль keeper-side через vault('secret/redis/<inc>#password');
//   - MaterializeDestinies + default_destiny_source: apply:destiny резолвит git-URL
//     каждой destiny из keeper_settings (ADR-029);
//   - WaitSoulprintReported: redis-exporter/node-exporter читают soulprint.self.os.arch
//     keeper-side при рендере URL release-tarball-ов — ждём непустые факты ДО Create;
//   - AssertHostHTTPContains (host_state.endpoints): curl :9100/metrics в контейнере.
//
// Timeout 300s — apt-update + apt-get install redis-server + два tarball-fetch-а
// (node_exporter ~10MB, redis_exporter ~8MB) + extract + systemctl start. На
// холодном CI медленно (README example фиксирует ~3-5 минут).
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisLive_CreateWithNodeExporter(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: service/redis свёрнут в режим standalone (exporter вынесен — мониторинг отдельная сущность); L3b real-soul create переписать под новый standalone — .pm/tasks/2026-06-22-redis-consolidation")

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		ServiceName: "redis",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("ожидался 1 soul-контейнер, получено %d", got)
	}
	const wantSID = "soul-live-a.example.com"
	if sc := stack.SoulContainers[0]; sc.SID != wantSID {
		t.Errorf("SoulContainers[0].SID = %q, ожидалось %q", sc.SID, wantSID)
	}

	const incName = "redis"

	// Vault-seed redis-пароля: create читает его keeper-side через
	// vault('secret/redis/'+incarnation.name+'#password'). rel БЕЗ mount/`data/`.
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-redis-secret",
	})

	// Coven-членство ДО Create: roster резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Bootstrap-flow выставил status='connected', но coven пуст.
	stack.AddSoulToCoven(t, 0, incName)

	// Ждём первый SoulprintReport реального soul-а: redis-exporter/node-exporter
	// резолвят soulprint.self.os.arch keeper-side при рендере URL tarball-ов
	// (ADR-018). Реальный soul шлёт его сразу при установке сессии, но это
	// отдельное сообщение ПОСЛЕ status='connected' — без ожидания первый render
	// мог бы попасть на пустой soulprint_facts.
	stack.WaitSoulprintReported(t, 0, 60)

	// Материализуем три standalone-destiny под git-тегом v1.0.0 (ref из
	// service.yml::destiny[]) и ставим default_destiny_source. SeedDefaultDestinySource
	// блокируется на holderRefreshGrace, чтобы Holder подхватил значение ДО
	// create-render-а. redis-replication-config (только add_replicas) не нужна.
	stack.MaterializeDestinies(t, "v1.0.0", "redis-single", "redis-exporter", "node-exporter")

	// node_exporter_sha256 / redis_exporter_sha256 — required-input (fail-closed).
	// На L3b fetch РЕАЛЬНЫЙ → checksum-ы должны совпасть с GitHub-релизами под пару
	// (version, arch=amd64). node_exporter 1.8.2 linux-amd64 + redis_exporter 1.62.0
	// linux-amd64 — значения из sha256sums соответствующих GitHub-релизов.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis@main", map[string]any{
		"redis_version":         "", // latest из репо дистрибутива (без =version-пина)
		"node_exporter_sha256":  nodeExporterSHA256,
		"redis_exporter_sha256": redisExporterSHA256,
	})

	stack.WaitApplySuccess(t, applyID, 300)
	// apply_runs success ≠ state закоммичен — ждём ready перед чтением state.
	stack.WaitIncarnationReady(t, inc, 30)

	exp := harness.LoadExpectations(t, "redis/expectations/after-create.yaml")
	stack.AssertExpectations(t, exp, applyID, inc)

	// apply_id в payload audit-event-а — runtime-значение, отдельно от YAML-fixture.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
}

// SHA-256 release-tarball-ов под (version, arch=amd64) из sha256sums GitHub-
// релизов. Обязательны (required-input, fail-closed): на L3b fetch реальный, и
// core.url.fetched верифицирует контрольную сумму ДО публикации (supply-chain).
//
//   - node_exporter 1.8.2  linux-amd64 → prometheus/node_exporter v1.8.2 sha256sums;
//   - redis_exporter 1.62.0 linux-amd64 → oliver006/redis_exporter v1.62.0 sha256sums.
const (
	nodeExporterSHA256  = "sha256:6809dd0b3ec45fd6e992c19071d6b5253aed3ead7bf0686885a51d85c6643c66"
	redisExporterSHA256 = "sha256:a09f92a6b366e37c654e50522c7b80e4a625396b2499fd42cf17e1aa91e56d5e"
)
