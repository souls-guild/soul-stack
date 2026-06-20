//go:build e2e

// L3a E2E pilot: examples/service/redis::create (ADR-039).
//
// Первый e2e под service-композицию через apply:destiny (ADR-009): scenario
// create раскрывает три standalone-destiny (redis-single → redis-exporter →
// node-exporter) изолированным render-проходом каждой. По сравнению со
// smoke-nginx/hello-world этот pilot вскрывает несколько keeper-side-механик,
// которые L3a-harness раньше не поддерживал и которые задокументированы в
// harness-доработках этого среза:
//
//   - default_destiny_source + материализация destiny-репо (harness.MaterializeDestinies):
//     apply:destiny резолвит git-URL каждой destiny из keeper_settings, fixture
//     раньше материализовал только сам сервис;
//   - Vault KV-seed (harness.SeedVaultKV): create читает пароль redis keeper-side
//     через CEL vault('secret/redis/<inc>#password') в render-фазе;
//   - soulprint-seed (Stack.SeedSoulprint): redis-exporter/node-exporter читают
//     soulprint.self.os.arch keeper-side при рендере URL release-tarball-ов.
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper-процесс + 1 soul-stub.
//  2. Seed Vault (redis-пароль) + soulprint(os.arch) + Coven-членство.
//  3. MaterializeDestinies(redis-single, redis-exporter, node-exporter) +
//     RegisterService(redis) → реестр сервисов + default_destiny_source.
//  4. ConnectSoulStub + LoadApplyScript (scripted success по task-name).
//  5. CreateIncarnationWithApply → авто-create-прогон → WaitApplySuccess.
//  6. Asserts: apply_runs success / incarnation.state (версии/socket/users) /
//     audit incarnation.created / metric keeper_scenario_runs_total{result="ok"}.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceRedis_Create(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		Souls:       1,
	})
	defer stack.Cleanup()

	const incName = "redis"

	// Vault-seed: create читает пароль redis keeper-side через
	// vault('secret/redis/'+incarnation.name+'#password'). Путь строится из
	// incarnation.name (= "redis"), поле password. rel БЕЗ mount/`data/`-префикса —
	// SeedVaultKV добавляет их (KV v2). Без секрета render-фаза падает
	// «vault-ref: KV path not found».
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-redis-secret",
	})

	// soulprint-seed: redis-exporter/node-exporter резолвят arch из
	// soulprint.self.os.arch keeper-side. amd64 → release-tarball linux-amd64.
	// pkg_mgr/init_system нужны core.pkg/core.service (ADR-018, soulprint.md §3).
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
	// 10s TTL-poll-а. redis-replication-config объявлена в service.yml::destiny[],
	// но create её НЕ использует (только add_replicas) — не материализуем.
	stack.MaterializeDestinies(t, "v1.0.0", "redis-single", "redis-exporter", "node-exporter")
	stack.RegisterService(t, "redis", "examples/service/redis")

	// Live EventStream-стрим: захват Redis SID-lease → ApplyRequest
	// смаршрутизируется в локальный Outbound. LoadApplyScript — scripted success
	// по task-name создаваемых задач (+ default-success для when:-collector-задач).
	stub := stack.ConnectSoulStub(t, 0)
	harness.LoadApplyScript(stub, "create", redisCreateTasks())

	// node_exporter_sha256 / redis_exporter_sha256 — required-input (fail-closed,
	// без дефолта). На L3a fetch не выполняется (soul-stub) — передаём валидные по
	// паттерну sha256:<64hex> плейсхолдеры, чтобы пройти input-validation+render.
	// redis_version — непустой distro-native пин (для непустого state.redis_version).
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis@main", map[string]any{
		"redis_version":         "5:7.0.15-1~deb12u7",
		"node_exporter_sha256":  "sha256:" + zeroHex64,
		"redis_exporter_sha256": "sha256:" + zeroHex64,
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	// apply_runs success ≠ incarnation.state закоммичен: state_changes пишутся
	// отдельной транзакцией ПОСЛЕ барьера (run.go §8). Ждём ready перед чтением.
	stack.WaitIncarnationReady(t, inc, 30)
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_version":          "5:7.0.15-1~deb12u7",
		"node_exporter_version":  "1.8.2",
		"redis_exporter_version": "1.62.0",
		"redis_socket":           "/var/run/redis/redis-server.sock",
		"redis_maxmemory":        "256mb",
		"redis_users":            []any{},
	})
	// POST /v1/incarnations авто-запускает create-scenario и пишет
	// incarnation.created с apply_id авто-прогона в payload (тот же applyID, что
	// в WaitApplySuccess). scenario_started — только при явном RunScenario.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// zeroHex64 — 64-символьная hex-строка для плейсхолдер-sha256 (паттерн
// `^sha256:[0-9a-f]{64}$` из input-схемы redis). Реальной верификации на L3a нет
// (fetch не выполняется), важна только валидность формата для input-validation.
const zeroHex64 = "0000000000000000000000000000000000000000000000000000000000000000"

// hex64Of повторяет цифру d 64 раза — валидный sha256-плейсхолдер для update_*
// сценариев (другой checksum, чем у create, чтобы read-обозримо отличался).
func hex64Of(d byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = d
	}
	return string(b)
}

// deployRedisCreate разворачивает redis::create на одном узле (Coven=coven) и
// блокируется до incarnation `ready`. Возвращает имя incarnation. Общий префикс
// мутирующих сценариев (add_acl_user/update_config/update_node_exporter/
// restart_node_exporter): они работают над уже-развёрнутой инкарнацией. stub
// должен быть уже подключён и заряжен (LoadApplyScript с задачами create +
// целевого сценария); default-success покрывает всё, что не в скрипте.
//
// Coven-метка = incName: roster прогона резолвится по `incarnation.name ∈ coven[]`
// (ADR-008), поэтому soul добавляется в Coven с именем incarnation.
func deployRedisCreate(t *testing.T, stack *harness.Stack, incName string) string {
	t.Helper()

	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-redis-secret",
	})
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
	stack.AddSoulToCoven(t, 0, incName)
	stack.MaterializeDestinies(t, "v1.0.0", "redis-single", "redis-exporter", "node-exporter")
	stack.RegisterService(t, "redis", "examples/service/redis")

	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis@main", map[string]any{
		"redis_version":         "5:7.0.15-1~deb12u7",
		"node_exporter_sha256":  "sha256:" + zeroHex64,
		"redis_exporter_sha256": "sha256:" + zeroHex64,
	})
	stack.WaitApplySuccess(t, applyID, 60)
	stack.WaitIncarnationReady(t, inc, 30)
	return inc
}

// TestE2EServiceRedis_AddAclUser — мутирующий сценарий add_acl_user поверх
// развёрнутой create-инкарнации. add_acl_user раскрывает loop по input.users в N
// RenderedTask core.cmd.shell (одно task_name на все итерации) и перезаписывает
// incarnation.state.redis_users целиком (state_changes.sets). Проверяем:
// apply_runs success → redis_users == переданный список → audit scenario_started →
// metric keeper_scenario_runs_total{result="ok"}.
func TestE2EServiceRedis_AddAclUser(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		Souls:       1,
	})
	defer stack.Cleanup()

	const incName = "redis-acl"

	stub := stack.ConnectSoulStub(t, 0)
	// Один скрипт на тест: задачи create (3 destiny) + задача add_acl_user. stub
	// матчит по task_name (findEntriesByTask), default-success покрывает остальное.
	harness.LoadApplyScript(stub, "redis-acl", append(redisCreateTasks(),
		harness.TaskResponse{TaskName: "Apply Redis ACL for each user in the list"},
	))

	deployRedisCreate(t, stack, incName)

	users := []any{
		map[string]any{"name": "app", "acl": "on >app-pass-1234 ~app:* +@read +@write"},
		map[string]any{"name": "readonly", "acl": "on >ro-pass-5678 ~* +@read"},
		map[string]any{"name": "metrics", "acl": "on >mx-pass-9012 +info +ping +client"},
	}
	applyID := stack.RunScenario(t, incName, "add_acl_user", map[string]any{
		"users": users,
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 30)
	stack.AssertIncarnationState(t, incName, map[string]any{
		"redis_users": users,
	})
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "add_acl_user",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// TestE2EServiceRedis_UpdateConfig — re-apply destiny redis-single с новым
// maxmemory + config (тот же изолированный render-проход, что в create, иной
// config). state_changes.sets фиксирует redis_maxmemory + redis_config.
func TestE2EServiceRedis_UpdateConfig(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		Souls:       1,
	})
	defer stack.Cleanup()

	const incName = "redis-cfg"

	stub := stack.ConnectSoulStub(t, 0)
	// Задачи update_config = задачи redis-single (та же destiny, что в create),
	// поэтому redisCreateTasks() уже покрывает их по task_name.
	harness.LoadApplyScript(stub, "redis-cfg", redisCreateTasks())

	deployRedisCreate(t, stack, incName)

	newConfig := map[string]any{
		"maxclients": 20000,
		"timeout":    0,
		"appendonly": "yes",
	}
	applyID := stack.RunScenario(t, incName, "update_config", map[string]any{
		"redis_maxmemory": "1gb",
		"config":          newConfig,
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 30)
	stack.AssertIncarnationState(t, incName, map[string]any{
		"redis_maxmemory": "1gb",
		"redis_config":    newConfig,
	})
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "update_config",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// TestE2EServiceRedis_UpdateNodeExporter — точечный re-apply destiny node-exporter
// с НОВОЙ версией (1.9.0; create ставил 1.8.2). state_changes.sets фиксирует
// ТОЛЬКО node_exporter_version — мутация версии доказывается subset-ом
// node_exporter_version=1.9.0 (остальной state от create неизменен).
func TestE2EServiceRedis_UpdateNodeExporter(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		Souls:       1,
	})
	defer stack.Cleanup()

	const incName = "redis-nodeexp"

	stub := stack.ConnectSoulStub(t, 0)
	// Задачи update_node_exporter = задачи node-exporter (та же destiny, что в
	// create) → redisCreateTasks() покрывает их по task_name.
	harness.LoadApplyScript(stub, "redis-nodeexp", redisCreateTasks())

	deployRedisCreate(t, stack, incName)

	applyID := stack.RunScenario(t, incName, "update_node_exporter", map[string]any{
		"node_exporter_version": "1.9.0",
		// Другой sha256-плейсхолдер, чем у create (формат валиден для input-validation;
		// fetch на L3a не выполняется).
		"node_exporter_sha256": "sha256:" + hex64Of('3'),
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 30)
	stack.AssertIncarnationState(t, incName, map[string]any{
		"node_exporter_version": "1.9.0",
	})
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "update_node_exporter",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// TestE2EServiceRedis_RestartNodeExporter — принудительный рестарт демона
// node_exporter без мутации incarnation.state (сценарий не объявляет
// state_changes). Проверяем lifecycle: apply_runs success → audit scenario_started
// → metric. Дельты state нет — assert на incarnation.state не делаем.
func TestE2EServiceRedis_RestartNodeExporter(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		Souls:       1,
	})
	defer stack.Cleanup()

	const incName = "redis-restart"

	stub := stack.ConnectSoulStub(t, 0)
	harness.LoadApplyScript(stub, "redis-restart", append(redisCreateTasks(),
		harness.TaskResponse{TaskName: "Restart node_exporter"},
	))

	deployRedisCreate(t, stack, incName)

	applyID := stack.RunScenario(t, incName, "restart_node_exporter", nil)

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	// state_changes отсутствует → incarnation сразу возвращается в ready без
	// мутации state; ждём ready как маркер завершения commit-ветки.
	stack.WaitIncarnationReady(t, incName, 30)
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "restart_node_exporter",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// TestE2EServiceRedis_AddReplicas — runtime-масштабирование репликами.
//
// SKIP на L3a (документировано). add_replicas резолвит register.master_addr
// (результат probe-задачи "Detect actual redis primary…") в apply:input шага
// replicaof. Keeper рендерит весь сценарий ОДНИМ up-front проходом ДО dispatch-а,
// а register в этот render не кладётся — его строит реальный Soul из результатов
// предыдущих задач (keeper/internal/render/pipeline.go, комментарий «register в
// flow_context НЕ кладётся»). soul-stub register-данные на обычный ApplyRequest
// не эмитит (только dry_run-Plan), поэтому свёртка
// `register.master_addr.map(...).filter(v,v!=”)[0]` рендерится по пустому
// register. Это runtime-probe + cross-host register — территория L3b (реальный
// Soul исполняет probe). Фикстуры (souls-/stub-responses-/after-add-replicas.yaml)
// описывают желаемый L3b-результат; здесь тест явно Skip-ается, чтобы не давать
// ложный red и фиксировать сценарий как покрытый-но-отложенный.
//
// Дополнительно: даже в L3b incarnation.state.redis_hosts НЕ появится в этой
// версии — add_replicas объявляет state_changes.appends, а appends/modifies в
// commit пока не применяются (state.go/pipeline.go: «future-расширение»).
func TestE2EServiceRedis_AddReplicas(t *testing.T) {
	t.Skip("add_replicas требует runtime-probe + cross-host register (master_addr из probe-результата в apply:input на single-pass keeper-render); soul-stub L3a register на обычный ApplyRequest не эмитит — это L3b. См. fixtures/souls-add-replicas.yaml.")
}

// redisCreateTasks — scripted success-ответы по task-name всех материализуемых
// задач create (три destiny). Зеркало
// tests/e2e/redis/fixtures/stub-responses.yaml::scenarios.create.apply_responses
// (загружается inline — YAML-loader fixtures не реализован, pilot-паттерн).
func redisCreateTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		// destiny redis-single.
		{TaskName: "Install redis-server package", StateChanges: map[string]any{"packages": []any{map[string]any{"redis-server": "installed"}}}},
		{TaskName: "Ensure the redis socket directory exists"},
		{TaskName: "Render redis.conf with dual-access (TCP + unix socket)"},
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
