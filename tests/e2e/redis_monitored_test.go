//go:build e2e

// L3a E2E: examples/service/redis-monitored::create (ADR-039).
//
// redis-monitored — второй сервис-композит через apply:destiny (ADR-009): scenario
// create раскрывает те же три standalone-destiny, что и examples/service/redis
// (redis-single → redis-exporter → node-exporter), изолированным render-проходом
// каждой. Отличия от pilot-а tests/e2e/redis, ради которых этот тест и нужен:
//
//   - пароль Redis НЕ из Vault: redis-monitored принимает его как required input
//     redis_password (литерал/scoped vault-ref), не читает keeper-side через
//     vault('secret/redis/...'). → SeedVaultKV здесь НЕ вызывается;
//   - state_schema уже: service.yml объявляет ровно 4 поля (redis_version,
//     node_exporter_version, redis_exporter_version, redis_socket) — без
//     redis_maxmemory/redis_users, которые есть у redis. assert incarnation.state
//     проверяет именно этот контракт;
//   - required-input: помимо двух checksum-ов экспортеров (как у redis) обязателен
//     redis_password (min_length 16).
//
// soulprint(os.arch) по-прежнему обязателен: redis-exporter/node-exporter читают
// soulprint.self.os.arch keeper-side при рендере URL release-tarball-ов (ADR-018).
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper-процесс + 1 soul-stub.
//  2. soulprint(os.arch=amd64) + Coven-членство (incarnation.name = redis-monitored).
//  3. MaterializeDestinies(redis-single, redis-exporter, node-exporter) +
//     RegisterService(redis-monitored) → реестр сервисов + default_destiny_source.
//  4. ConnectSoulStub + LoadApplyScript (scripted success по task-name).
//  5. CreateIncarnationWithApply (redis_password + два sha256) → авто-create-прогон.
//  6. Asserts: apply_runs success / incarnation.state (4 поля контракта) /
//     audit incarnation.created / metric keeper_scenario_runs_total{result="ok"}.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceRedisMonitored_Create(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis-monitored",
		Souls:       1,
	})
	defer stack.Cleanup()

	const incName = "redis-monitored"

	// soulprint-seed: redis-exporter/node-exporter резолвят arch из
	// soulprint.self.os.arch keeper-side. amd64 → release-tarball linux-amd64.
	// pkg_mgr/init_system нужны core.pkg/core.service (ADR-018, soulprint.md §3).
	// Vault НЕ сеется: пароль Redis приходит из input (см. шапку файла).
	stack.SeedSoulprint(t, 0, map[string]any{
		"os": map[string]any{
			"family":      "debian",
			"distro":      "debian",
			"version":     "12",
			"arch":        "amd64",
			"pkg_mgr":     "apt",
			"init_system": "systemd",
		},
		"hostname": "soul-a",
	})

	// Coven-членство ДО Create: roster резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Без него scenario видит no_hosts → error_locked.
	stack.AddSoulToCoven(t, 0, incName)

	// Материализуем три standalone-destiny в file://-репо и ставим
	// keeper_settings[default_destiny_source]=file://.../{name}. ДО RegisterService:
	// invalidate от POST /v1/services подтянет настройку в Holder без ожидания
	// 10s TTL-poll-а. Имена/ref — ровно те, что в service.yml::destiny[] (v1.0.0).
	stack.MaterializeDestinies(t, "v1.0.0", "redis-single", "redis-exporter", "node-exporter")
	stack.RegisterService(t, "redis-monitored", "examples/service/redis-monitored")

	// Live EventStream-стрим: захват Redis SID-lease → ApplyRequest
	// смаршрутизируется в локальный Outbound. LoadApplyScript — scripted success
	// по task-name создаваемых задач (+ default-success для when:-collector-задач).
	stub := stack.ConnectSoulStub(t, 0)
	harness.LoadApplyScript(stub, "create", redisMonitoredCreateTasks())

	// Required-input redis-monitored:
	//   - redis_password (secret, min_length 16) — здесь литерал;
	//   - node_exporter_sha256 / redis_exporter_sha256 (fail-closed, без дефолта):
	//     на L3a fetch не выполняется (soul-stub) — валидные по паттерну
	//     sha256:<64hex> плейсхолдеры, чтобы пройти input-validation+render.
	// redis_version — непустой distro-native пин (для непустого state.redis_version
	// через has(...)-guard; передан пустым дал бы state "").
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis-monitored@main", map[string]any{
		"redis_password":        "monitored-redis-secret-32",
		"redis_version":         "5:7.0.15-1~deb12u7",
		"node_exporter_sha256":  "sha256:" + zeroHexMon64,
		"redis_exporter_sha256": "sha256:" + zeroHexMon64,
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	// apply_runs success ≠ incarnation.state закоммичен: state_changes пишутся
	// отдельной транзакцией ПОСЛЕ барьера (run.go §8). Ждём ready перед чтением.
	stack.WaitIncarnationReady(t, inc, 30)
	// state_schema redis-monitored — ровно 4 поля (service.yml::state_schema).
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_version":          "5:7.0.15-1~deb12u7",
		"node_exporter_version":  "1.8.2",
		"redis_exporter_version": "1.62.0",
		"redis_socket":           "/var/run/redis/redis-server.sock",
	})
	// POST /v1/incarnations авто-запускает create-scenario и пишет
	// incarnation.created с apply_id авто-прогона в payload (тот же applyID, что
	// в WaitApplySuccess). scenario_started — только при явном RunScenario.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// zeroHexMon64 — 64-символьная hex-строка для плейсхолдер-sha256 (паттерн
// `^sha256:[0-9a-f]{64}$` из input-схемы redis-monitored). Реальной верификации
// на L3a нет (fetch не выполняется), важна только валидность формата для
// input-validation. Отдельное имя от redis_test.go::zeroHex64 — оба файла в
// пакете e2e_test, литерал-константы не должны коллидировать.
const zeroHexMon64 = "0000000000000000000000000000000000000000000000000000000000000000"

// redisMonitoredCreateTasks — scripted success-ответы по task-name всех
// материализуемых задач create (три destiny). Зеркало
// tests/e2e/redis-monitored/fixtures/stub-responses.yaml::scenarios.create.apply_responses
// (загружается inline — YAML-loader fixtures не реализован, pilot-паттерн).
//
// task-name идентичны redis_test.go::redisCreateTasks (те же три destiny), но
// функция отдельная: явное зеркало именно этого fixture-файла, без
// cross-file-зависимости между тестами одного пакета.
func redisMonitoredCreateTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		// destiny redis-single.
		{TaskName: "Install redis-server package", StateChanges: map[string]any{"packages": []any{map[string]any{"redis-server": "installed"}}}},
		{TaskName: "Ensure the redis socket directory exists"},
		{TaskName: "Render redis.conf with dual-access (TCP + unix socket)"},
		{TaskName: "Ensure the redis-server systemd drop-in directory exists"},
		{TaskName: "Render redis-server systemd hardening drop-in"},
		{TaskName: "Reload systemd because the hardening drop-in changed"},
		{TaskName: "Ensure redis-server is running and enabled at boot", StateChanges: map[string]any{"services": []any{map[string]any{"redis-server": "running"}}}},
		{TaskName: "Restart redis-server because config or hardening changed"},
		// destiny redis-exporter.
		{TaskName: "Fetch redis_exporter tarball"},
		{TaskName: "Extract redis_exporter tarball"},
		{TaskName: "Install redis_exporter binary"},
		{TaskName: "Render redis_exporter systemd unit"},
		{TaskName: "Ensure redis_exporter is running and enabled at boot", StateChanges: map[string]any{"services": []any{map[string]any{"redis_exporter": "running"}}}},
		{TaskName: "Restart redis_exporter because unit changed"},
		// destiny node-exporter.
		{TaskName: "Fetch node_exporter tarball"},
		{TaskName: "Extract node_exporter tarball"},
		{TaskName: "Ensure the node_exporter system group exists"},
		{TaskName: "Ensure the node_exporter system user exists"},
		{TaskName: "Ensure textfile collector directory exists"},
		{TaskName: "Install node_exporter binary"},
		{TaskName: "Render node_exporter systemd unit"},
		{TaskName: "Ensure node_exporter is running and enabled at boot", StateChanges: map[string]any{"services": []any{map[string]any{"node_exporter": "running"}}}},
		{TaskName: "Restart node_exporter because unit or binary changed"},
	}
}
