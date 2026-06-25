package audit

// EventType — typed alias для колонки `audit_log.event_type`.
// Convention имени — `<area>.<action>` (lowercase, dots; см.
// docs/naming-rules.md → Audit-events).
//
// Каталог открытый: новые имена добавляются обычным PR в
// docs/naming-rules.md при нормировании write-path-подсистемы. Здесь
// типизированно объявлены только те имена, которые уже зафиксированы
// ADR (ADR-021 → config.*). Остальные области (`operator.*`,
// `incarnation.*`, `push.*`, `cloud.*`, `reaper.*`, `task.*`,
// `soulprint.*`) добавляются в M0.4.2+ по факту имплементации
// инициатора.
type EventType string

const (
	// EventConfigReloadSucceeded — после успешного atomic swap нового
	// конфига (ADR-021(g)). payload: `changed_paths`, `correlation_id`.
	EventConfigReloadSucceeded EventType = "config.reload_succeeded"

	// EventConfigReloadFailed — после провалившейся валидации
	// (in-memory state неизменен, файл не модифицирован) (ADR-021(g)).
	// payload: `validation_errors[]`, `phase`.
	EventConfigReloadFailed EventType = "config.reload_failed"

	// EventOperatorCreated — создан новый Архонт. Bootstrap первого
	// Архонта (`keeper init`, ADR-013) пишет это событие с
	// `source: keeper_internal`, `archon_aid: NULL` и
	// `payload.bootstrap_initial: true`; последующие `operator.create`
	// через Operator API (M0.6+) — с `source: api` / `archon_aid`
	// создателя и `bootstrap_initial: false` (omitted).
	EventOperatorCreated EventType = "operator.created"

	// EventOperatorRevoked — Архонт ревокнут через Operator API
	// (`POST /v1/operators/{aid}/revoke`). `source: api`, `archon_aid` —
	// инициатор; payload: `{aid, reason}`. Активные JWT ревокнутого
	// Архонта продолжают работать до `exp` (ADR-014(d)).
	EventOperatorRevoked EventType = "operator.revoked"

	// EventOperatorTokenIssued — выпущен новый JWT для существующего
	// Архонта через Operator API (`POST /v1/operators/{aid}/issue-token`).
	// `source: api`, `archon_aid` — инициатор; payload: `{aid, expires_at}`.
	// JWT в payload НЕ кладётся (sensitive, masked даже если попадёт).
	EventOperatorTokenIssued EventType = "operator.token-issued"

	// EventIncarnationCreated — создан новый runtime-инстанс через Operator
	// API (`POST /v1/incarnations`). `source: api`, `archon_aid` —
	// инициатор; payload: `{name, service, apply_id}`. M0.6c-1 — stub:
	// audit пишется при insert-е row-а incarnation, реальный запуск
	// scenario `create` блокирован M2.x (Soul gRPC infrastructure).
	EventIncarnationCreated EventType = "incarnation.created"

	// EventIncarnationScenarioStarted — оператор запустил именованный
	// scenario против существующей incarnation через Operator API
	// (`POST /v1/incarnations/{name}/scenarios/{scenario}`, ADR-009).
	// `source: api`, `archon_aid` — инициатор; payload: `{name, scenario,
	// apply_id}`. Async: audit пишется при приёме запроса (202); терминал
	// прогона фиксируется отдельным `run.completed` (M2.4).
	EventIncarnationScenarioStarted EventType = "incarnation.scenario_started"

	// EventIncarnationUnlocked — оператор снял статус error_locked через
	// Operator API (`POST /v1/incarnations/{name}/unlock`, ADR-009).
	// `source: api`, `archon_aid` — инициатор; payload: `{name,
	// previous_status, reason}`. Unlock НЕ откатывает и НЕ доделывает
	// хосты — только снимает блок, оператор берёт ответственность за
	// консистентность (architecture.md → «Атомарность и error_locked»).
	EventIncarnationUnlocked EventType = "incarnation.unlocked"

	// EventIncarnationCreateRerun — оператор перезапустил scenario `create` из
	// error_locked через Operator API / MCP (`POST /v1/incarnations/{name}/
	// rerun-create`, architecture.md → «Атомарность и error_locked»).
	// `source: api` / `mcp`, `archon_aid` — инициатор; payload: `{name, reason,
	// previous_status, apply_id}`. Атомарно снимает error_locked (state НЕ
	// трогается — last known-good, snapshot в state_history) и тем же действием
	// запускает scenario `create` (переход error_locked → applying минуя ready
	// под одним FOR UPDATE). Отдельное событие от `incarnation.unlocked` (ручной
	// unlock не перезапускает прогон) — путь восстановления различен.
	EventIncarnationCreateRerun EventType = "incarnation.create_rerun"

	// EventIncarnationUpgradeStarted — оператор инициировал перевод
	// incarnation на новую state_schema_version через Operator API
	// (`POST /v1/incarnations/{name}/upgrade`, ADR-019). `source: api`,
	// `archon_aid` — инициатор; payload: `{name, to_version, apply_id}`.
	// sync-под-202: миграция выполняется синхронно в рамках запроса (одна
	// PG-транзакция, docs/migrations.md §Атомарность), audit пишется при
	// приёме запроса; per-step state_history-snapshot-ы фиксируются внутри
	// той же tx с общим apply_id.
	EventIncarnationUpgradeStarted EventType = "incarnation.upgrade_started"

	// EventIncarnationDestroyStarted — оператор инициировал destroy
	// incarnation через Operator API / MCP (S-D1). `source: api` / `mcp`,
	// `archon_aid` — инициатор; payload: `{name, previous_status, force}`.
	// Пишется при переводе incarnation в `destroying` (до запуска teardown
	// scenario `destroy` — S-D2, и DELETE строки — S-D3). `force: true`
	// означает destroy без teardown (DELETE напрямую, S-D3). Терминал самого
	// destroy фиксируется `incarnation.destroy_completed` / `.destroy_failed`.
	EventIncarnationDestroyStarted EventType = "incarnation.destroy_started"

	// EventIncarnationDestroyCompleted — destroy доведён до конца: teardown
	// прошёл на всех хостах, строка incarnation физически снесена с архивом
	// в incarnation_archive / state_history_archive (S-D3, каскад V3).
	// `source: keeper_internal` (write-path — scenario-runner после барьера,
	// не HTTP-middleware; AID инициатора недоступен в этой точке, archon_aid
	// колонка NULL). `correlation_id` — пусто. Payload: `{name, force}` —
	// факт сноса; секретов не несёт (state/spec в audit НЕ дублируются, лежат
	// в архиве). Пишется ПОСЛЕ commit-а archive+DELETE-транзакции;
	// single-winner — только владелец destroying-перехода пишет это событие
	// (RowsAffected==0 → no-op, событие не пишется).
	EventIncarnationDestroyCompleted EventType = "incarnation.destroy_completed"

	// EventIncarnationHostsUpdated — Архонт отредактировал declared `spec.hosts[]`
	// incarnation через Operator API (`PATCH /v1/incarnations/{name}/hosts`) —
	// поддерживает три mode: replace (полная замена списка), append (добавить /
	// обновить role по SID) и remove (убрать переданные SID-ы). `source: api` /
	// `mcp`, `archon_aid` — инициатор. Payload: `{name, mode, old_hosts,
	// new_hosts}` — `old_hosts`/`new_hosts` — снимок `spec.hosts[]` до и после
	// (SID + role, не секрет); mode фиксирует тип операции для диагностики.
	// declared `hosts` — источник probe-spec на bootstrap (ADR-008), правка
	// меняет namespacing topology resolver-а для следующего прогона.
	EventIncarnationHostsUpdated EventType = "incarnation.hosts_updated"

	// EventIncarnationDestroyFailed — teardown (scenario `destroy`) упал на
	// хостах: инстанс НЕ удалён, incarnation переведена в `destroy_failed`
	// (state остался last known-good). `source: keeper_internal` (write-path —
	// scenario-runner на провале teardown-а, archon_aid колонка NULL).
	// `correlation_id = apply_id`. Payload: `{name, apply_id, reason}` —
	// `reason` маскируется (cause может транзитом нести vault-ref). Симметрично
	// `incarnation.destroy_completed`: оба фиксируют терминал destroy,
	// отличаются исходом teardown-а.
	EventIncarnationDestroyFailed EventType = "incarnation.destroy_failed"

	// EventSoulCreated — Soul зарегистрирован в реестре `souls` через
	// Operator API (`POST /v1/souls`): создана строка (status: pending) и
	// для transport=agent выписан первый bootstrap-токен. `source: api`,
	// `archon_aid` — инициатор. Payload: `{sid, transport, covens,
	// created_by_aid, token_issued}` — plain-токен в payload НЕ кладётся
	// (sensitive; даже под ключом `bootstrap_token` был бы masked).
	EventSoulCreated EventType = "soul.created"

	// EventSoulTokenIssued — выпущен новый bootstrap-токен для существующей
	// Soul через Operator API (`POST /v1/souls/{sid}/issue-token`). `source:
	// api`, `archon_aid` — инициатор. Payload: `{sid, force, expired_previous,
	// expires_at}` — `expired_previous` = true, если force-reissue
	// инвалидировал ранее активный токен. Идентификаторы токенов в payload
	// НЕ кладутся: secret-mask (H1) редактирует любой ключ с `token`-substring,
	// корреляция идёт по sid + времени. Plain-токен тем более не кладётся.
	EventSoulTokenIssued EventType = "soul.token-issued"

	// EventSoulBootstrapped — Soul успешно прошёл онбординг через
	// `Bootstrap` gRPC RPC (docs/soul/onboarding.md): bootstrap-токен
	// сожжён, CSR подписан, SoulSeed выпущен и записан, статус Soul-а
	// переведён `pending → connected`. `source: soul_grpc`,
	// `archon_aid: NULL`, `correlation_id` = token_id. Payload: `{sid,
	// token_id, seed_id, fingerprint, not_after}`.
	EventSoulBootstrapped EventType = "soul.bootstrapped"

	// EventSoulSeedIssued — выпущен новый SoulSeed-сертификат (как часть
	// `Bootstrap` или через будущий `SeedRotation`-RPC M2.6). `source:
	// soul_grpc`, `archon_aid: NULL`. Payload: `{sid, seed_id, fingerprint,
	// serial_number, issued_at, not_after, kid}`. При bootstrap-е пишется
	// вместе с `soul.bootstrapped` (один correlation_id); при ротации —
	// самостоятельно.
	EventSoulSeedIssued EventType = "soul.seed-issued"

	// EventTaskExecuted — завершилась задача apply-прогона. Единое имя для всех
	// terminal-статусов (`ok`/`changed`/`failed`/`timed_out`/`skipped`) — status
	// выносится в `payload.status`, чтобы фильтрация в `GET /v1/audit` шла по нему,
	// а не по разбегу event_type. `correlation_id = apply_id`. Payload (общая форма
	// [BuildTaskExecutedPayload]): `{sid, apply_id, task_idx, status, error?,
	// register_data?}` — `error` только при FAILED/TIMED_OUT; `register_data`
	// маскируется по общим правилам секретов.
	//
	// Эмитируется ОБЕИМИ сторонами (ADR-052 amend §k/§l): Soul-side задачи — M2.4
	// event handler `TaskEvent`, `source: soul_grpc`, `sid` хоста; keeper-side
	// задачи `on: keeper` (`scenario.dispatchKeeperTasks`) — `source:
	// keeper_internal`, `sid = keeper`. Без keeper-side эмиссии changed-keeper-
	// задача выпадала бы из свёртки changed_tasks и task-подписки Tiding.
	// keeper-side payload register_data НЕ несёт (секрет-гигиена).
	EventTaskExecuted EventType = "task.executed"

	// EventRunCompleted — финальный отчёт прогона apply (M2.4 event handler
	// `RunResult`). Единое имя для всех RunStatus-ов
	// (`success`/`failed`/`cancelled`/`error_locked`) — статус в
	// `payload.status`. `source: soul_grpc`, `correlation_id = apply_id`.
	// Payload: `{sid, apply_id, status, incarnation?, scenario?, history_id?}`.
	EventRunCompleted EventType = "run.completed"

	// EventIncarnationRunCompleted — терминал scenario-run одной инкарнации
	// (T3/T4-фундамент, ADR-052 §k): per-incarnation итог прогона, эмитится
	// scenario.Runner на терминале обычного прогона. Одно событие на
	// инкарнацию-прогон, НЕ per-host (развести с `run.completed` — тот per-host
	// RunResult от Soul-а). `source: keeper_internal` (write-path — scenario-
	// runner, archon_aid колонка NULL), `correlation_id = apply_id`.
	//
	// Эмитится ДВУМЯ путями: на УСПЕШНОМ финале (после барьера, рядом с
	// commitSuccess) со `status: success` И на ТЕРМИНАЛЬНОМ ПРОВАЛЕ обычного
	// прогона (после lockIncarnation, only single-winner) со `status: failed`.
	// TerminalDestroy в обе точки НЕ приходит — у destroy свой терминал
	// (`incarnation.destroy_completed` / `.destroy_failed`).
	//
	// Payload: `{incarnation, scenario, apply_id, status, changed_tasks,
	// cadence_id?, voyage_id?}`. `status` ∈ {`success`, `failed`} (error_locked
	// сворачивается в `failed` — под-статусы не плодим). `changed_tasks` = массив
	// `{idx, name, register, id, module, changed_hosts, total_hosts}` задач,
	// изменившихся хотя бы на одном хосте (source = агрегат audit_log по
	// `task.executed`+CHANGED, loop-свёртка по адресу register∪id, ADR-052 §j); на
	// провале — частичный (поздний abort, что успело CHANGED) либо пустой (ранний
	// abort до render). `cadence_id` присутствует ТОЛЬКО когда прогон спавнен
	// Cadence-расписанием (дочерний Voyage, ADR-046) — ручной прогон ключ не несёт
	// (консервативно, как drift-payload). `voyage_id` присутствует ТОЛЬКО когда
	// прогон спавнен Voyage-orchestrator-ом (ADR-052 amend §k, visibility) —
	// прямые пути create/rerun/destroy минуют Voyage и ключ не несут (симметрия с
	// cadence_id); фильтруется в GET /v1/audit через `payload_voyage` для Voyage
	// detail. Секрет-гигиена: payload changed_tasks несёт ТОЛЬКО метаданные задачи
	// и counts, register/params-значения в нём отсутствуют.
	EventIncarnationRunCompleted EventType = "incarnation.run_completed"

	// EventSoulprintReceived — получен `SoulprintReport` от Soul-а (ADR-018,
	// M2.4 event handler `SoulprintReport`). `source: soul_grpc`,
	// `correlation_id` — пусто (это не часть apply-цепочки). Payload:
	// `{sid, collected_at, received_at, has_typed_facts}` — сами факты НЕ
	// дублируются в audit, лежат в `souls.soulprint_facts`.
	EventSoulprintReceived EventType = "soulprint.received"

	// EventInputVaultResolved — scenario-runner резолвил (или отверг)
	// `vault:`-ref в operator-input через scoped-канал (docs/input.md →
	// «vault_scope»). `source: keeper_internal`, `archon_aid: NULL` (write-path
	// — async scenario-runner, не HTTP-middleware; aid инициатора — в payload).
	// Единое имя для ok и denied — результат в `payload.result`
	// (`ok`/`denied`), чтобы фильтрация шла по нему, а не по разбегу
	// event_type. denied-резолв — security-сигнал, аудируется наравне с ok.
	// Payload: `{field, incarnation, scenario, result, aid?, path?, reason?}` —
	// `path` (логический путь Vault) НЕ секрет, логируется; значение секрета НЕ
	// кладётся; `reason` (`no_scope`/`out_of_scope`/`deny_list`/…) только при
	// denied.
	EventInputVaultResolved EventType = "input.vault_resolved"

	// EventVaultKVRead — keeper-side core-модуль `core.vault.kv-read`
	// (ADR-017) прочитал секрет из Vault KV. `source: keeper_internal`,
	// `archon_aid: NULL`. Payload: `{path, fields}` — путь и список
	// запрошенных ключей; сами значения секретов в payload **не** кладутся
	// (sensitive, audit-trail фиксирует факт чтения, не содержимое).
	EventVaultKVRead EventType = "vault.kv-read"

	// EventSoulCovenChanged — изменён набор Coven-меток Soul-а. Два write-path-а
	// различаются полем `source`:
	//   - scenario-путь: keeper-side core-модуль `core.soul.registered`
	//     (docs/keeper/modules.md), per-host. `source: keeper_internal`,
	//     `archon_aid: NULL`. Payload: `{sid, mode, before, after, created}`.
	//     Пишется только если набор фактически изменился.
	//   - bulk-API: `POST /v1/souls/coven` (массовое append/remove одной метки
	//     по селектору). `source: api`, `archon_aid` — инициатор (из claims,
	//     кладёт audit-middleware). Один event на всю операцию (не per-chunk).
	//     Payload: `{mode, label, selector, matched, changed, status,
	//     scope_applied, dry_run, source}`.
	EventSoulCovenChanged EventType = "soul.coven-changed"

	// EventSoulTraitsChanged — изменён набор operator-set trait-меток Soul-а
	// (jsonb-колонка `souls.traits`, ADR-060) через bulk-API
	// `POST /v1/souls/traits` (массовое merge/replace/remove по селектору).
	// `source: api`, `archon_aid` — инициатор (из claims, кладёт audit-middleware).
	// Один event на всю операцию (не per-chunk). Payload: `{mode, selector,
	// matched, changed, status, scope_applied, dry_run, source, keys}` — `keys`
	// = список затронутых trait-КЛЮЧЕЙ (для merge/replace — ключи переданного
	// набора; для remove — удаляемые ключи); сами trait-ЗНАЧЕНИЯ в payload НЕ
	// кладутся (могут нести инфраструктурные данные хоста — audit-trail фиксирует
	// факт мутации и набор ключей, не содержимое). Симметрично
	// `soul.coven-changed`, отдельная ось меток.
	EventSoulTraitsChanged EventType = "soul.traits-changed"

	// EventCloudProvisioned — keeper-side core-модуль `core.cloud.provisioned`
	// (ADR-017) создал или удалил VM через CloudDriver-плагин. `source:
	// keeper_internal`, `archon_aid: NULL`. Payload:
	// `{action, provider, profile, count, vm_ids}` — `action` ∈
	// `created`/`destroyed`. Cloud-credentials не кладутся.
	EventCloudProvisioned EventType = "cloud.provisioned"

	// EventApplyDispatched — Keeper отправил `ApplyRequest` Soul-у через
	// EventStream (M2.5, outbound direction). `source: soul_grpc`,
	// `archon_aid: NULL`, `correlation_id = apply_id`. Payload:
	// `{sid, apply_id, tasks_count}` — список задач не дублируем, он
	// материализуется через `task.executed` событиями по мере прогона.
	EventApplyDispatched EventType = "apply.dispatched"

	// EventApplyCancelled — Keeper отправил `CancelApply` Soul-у (M2.5).
	// `source: soul_grpc`, `archon_aid: NULL`, `correlation_id = apply_id`.
	// Payload: `{sid, apply_id, reason}`. Soul-side обработка (фактическая
	// отмена в-flight ApplyRunner-а) фиксируется отдельным `run.completed`
	// со `status: CANCELLED`.
	EventApplyCancelled EventType = "apply.cancelled"

	// EventLeaseForceReleased — Keeper-инстанс presence-gated перехватил
	// SID-lease у ДОКАЗАННО-МЁРТВОГО prev-holder-а на reconnect-е Soul-а
	// (ADR-027 amend (n), recovery-backstop S2). Security-чувствительная
	// операция смены владения lease: prev-holder подтверждён мёртвым через
	// Conclave-presence ([redis.InstanceAlive]), затем CAS-by-prev-holder
	// перезахватил ключ. `source: soul_grpc`, `archon_aid: NULL`,
	// `correlation_id = sid`. Payload: `{sid, prev_kid, new_kid}`. Пишется
	// ТОЛЬКО при успешном force-release (split-brain-отказ / fail-safe не
	// аудируются — это штатное «отдать Soul-у ретраить»).
	EventLeaseForceReleased EventType = "eventstream.lease_force_released"

	// EventSoulSeedRotated — Soul инициировал ротацию seed-а через
	// `SeedRotationRequest` в EventStream, Keeper выпустил новый cert и
	// supersede-нул предыдущий active (M2.5, ADR-012). `source: soul_grpc`,
	// `archon_aid: NULL`. Payload: `{sid, seed_id, fingerprint,
	// serial_number, not_after, kid, superseded_seed_id?}`. Симметрично
	// `soul.seed-issued` (ADR-014), отличается только триггером — здесь
	// инициатор Soul, при bootstrap — Keeper-side flow.
	EventSoulSeedRotated EventType = "soul.seed-rotated"

	// EventRoleCreated — создана RBAC-роль через Operator API
	// (`POST /v1/roles`) или MCP-tool `keeper.role.create` (RBAC Slice 2).
	// Изменение авторизации обязательно аудируется (ADR-022). `source: api`
	// или `mcp`, `archon_aid` — инициатор. Payload: `{name, permissions,
	// created_by_aid}` — permission-строки не секрет, логируются.
	EventRoleCreated EventType = "role.created"

	// EventRoleDeleted — удалена RBAC-роль через Operator API
	// (`DELETE /v1/roles/{name}`) или MCP-tool `keeper.role.delete` (RBAC
	// Slice 2). `source: api` или `mcp`, `archon_aid` — инициатор. Payload:
	// `{name}`. Каскадом снесены permissions + membership роли.
	EventRoleDeleted EventType = "role.deleted"

	// EventRolePermissionsUpdated — заменён набор permission-ов RBAC-роли
	// через Operator API (`PATCH /v1/roles/{name}/permissions`) или MCP-tool
	// `keeper.role.update` (RBAC Slice 2, replace-семантика). `source: api`
	// или `mcp`, `archon_aid` — инициатор. Payload: `{name, permissions}` —
	// permission-строки не секрет, логируются.
	EventRolePermissionsUpdated EventType = "role.permissions-updated"

	// EventRoleOperatorGranted — Архонт привязан к RBAC-роли через Operator
	// API (`POST /v1/roles/{name}/operators`) или MCP-tool
	// `keeper.role.grant-operator` (RBAC Slice 2). `source: api` или `mcp`,
	// `archon_aid` — инициатор. Payload: `{name, aid, granted_by_aid}` — AID-ы
	// не секрет, логируются.
	EventRoleOperatorGranted EventType = "role.operator-granted"

	// EventRoleOperatorRevoked — Архонт отвязан от RBAC-роли через Operator
	// API (`DELETE /v1/roles/{name}/operators/{aid}`) или MCP-tool
	// `keeper.role.revoke-operator` (RBAC Slice 2). `source: api` или `mcp`,
	// `archon_aid` — инициатор. Payload: `{name, aid}` — AID-ы не секрет,
	// логируются.
	EventRoleOperatorRevoked EventType = "role.operator-revoked"

	// EventSynodCreated — создана Synod-группа (ADR-049) через Operator API
	// (`POST /v1/synods`) или MCP-tool `keeper.synod.create`. Изменение
	// RBAC-топологии обязательно аудируется (ADR-022). `source: api` или `mcp`,
	// `archon_aid` — инициатор. Payload: `{name, created_by_aid}`.
	EventSynodCreated EventType = "synod.created"

	// EventSynodUpdated — изменено описание Synod-группы (ADR-049 amend) через
	// Operator API (`PATCH /v1/synods/{name}`) или MCP-tool `keeper.synod.update`.
	// Меняется ТОЛЬКО description (name (PK) immutable); прав не выдаёт/не отнимает,
	// но мутация RBAC-топологии аудируется (ADR-022) симметрично synod.created.
	// `source: api` или `mcp`, `archon_aid` — инициатор. Payload:
	// `{name, description}` — описание не секрет, логируется.
	EventSynodUpdated EventType = "synod.updated"

	// EventSynodDeleted — удалена Synod-группа (ADR-049) через Operator API
	// (`DELETE /v1/synods/{name}`) или MCP-tool `keeper.synod.delete`. `source:
	// api` или `mcp`, `archon_aid` — инициатор. Payload: `{name}`. Каскадом
	// снесены membership + bundle группы.
	EventSynodDeleted EventType = "synod.deleted"

	// EventSynodOperatorAdded — Архонт добавлен в Synod-группу (ADR-049) через
	// Operator API (`POST /v1/synods/{name}/operators`) или MCP-tool
	// `keeper.synod.add-operator`. Член получает весь bundle ролей группы.
	// `source: api` или `mcp`, `archon_aid` — инициатор. Payload:
	// `{name, aid, added_by_aid}` — AID-ы не секрет.
	EventSynodOperatorAdded EventType = "synod.operator-added"

	// EventSynodOperatorRemoved — Архонт убран из Synod-группы (ADR-049) через
	// Operator API (`DELETE /v1/synods/{name}/operators/{aid}`) или MCP-tool
	// `keeper.synod.remove-operator`. `source: api` или `mcp`, `archon_aid` —
	// инициатор. Payload: `{name, aid}`.
	EventSynodOperatorRemoved EventType = "synod.operator-removed"

	// EventSynodRoleGranted — роль добавлена в bundle Synod-группы (ADR-049)
	// через Operator API (`POST /v1/synods/{name}/roles`) или MCP-tool
	// `keeper.synod.grant-role`. Все члены группы получают эффективные права
	// роли. `source: api` или `mcp`, `archon_aid` — инициатор. Payload:
	// `{name, role, granted_by_aid}`.
	EventSynodRoleGranted EventType = "synod.role-granted"

	// EventSynodRoleRevoked — роль снята из bundle Synod-группы (ADR-049) через
	// Operator API (`DELETE /v1/synods/{name}/roles/{role_name}`) или MCP-tool
	// `keeper.synod.revoke-role`. Права роли снимаются у всех членов группы.
	// `source: api` или `mcp`, `archon_aid` — инициатор. Payload:
	// `{name, role}`.
	EventSynodRoleRevoked EventType = "synod.role-revoked"

	// EventPluginAllowed — Архонт допустил плагин в allow-list `plugin_sigils`
	// (Sigil, ADR-026) через Operator API (`POST /v1/plugins/sigils`) или
	// MCP-tool (S4b). `source: api` или `mcp`, `archon_aid` — инициатор.
	// Payload: `{namespace, name, ref, sha256, allowed_by_aid}` — supply-chain-
	// событие, обязательно аудируется; signature/manifest в payload НЕ кладутся
	// (крипто-материал / крупный JSONB). `ref` — operator-asserted метка
	// (вариант C: НЕ git-verified), authority целостности — sha256+подпись.
	EventPluginAllowed EventType = "plugin.allowed"

	// EventPluginRevoked — Архонт отозвал ранее допущенный плагин из
	// `plugin_sigils` (бинарь перестаёт проходить Sigil-верификацию) через
	// Operator API (`DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}`) или
	// MCP-tool (S4b). `source: api` или `mcp`, `archon_aid` — инициатор.
	// Payload: `{namespace, name, ref}`. (`plugin.verify_failed` — host-side
	// событие верификации, вводится отдельно в S6.)
	EventPluginRevoked EventType = "plugin.revoked"

	// EventAugurFetchBrokered — Augur-брокер (delegate=false, MVP-1, ADR-025 /
	// augur.md §8) прочитал значение из внешней системы и вернул его Soul-у
	// inline. `source: soul_grpc`, `archon_aid: NULL`, `correlation_id =
	// apply_id`. Payload: `{sid, omen, query, request_id}` — фиксируется ФАКТ
	// чтения + Omen + query (логический путь, не секрет); само значение /
	// токен в payload НЕ кладётся (augur.md §8, secret-masking ADR-010).
	EventAugurFetchBrokered EventType = "augur.fetch_brokered"

	// EventAugurAccessDenied — любая проверка авторизации Augur-запроса
	// (augur.md §6) провалена: Omen не найден / Soul вне Rite / query вне
	// allow-list / нормализация vault-path отвергла запрос. denied-резолв —
	// security-сигнал, аудируется наравне с успехом. `source: soul_grpc`,
	// `archon_aid: NULL`, `correlation_id = apply_id`. Payload: `{sid, omen,
	// query, request_id, reason}` — `reason` человекочитаемая причина отказа;
	// значения секретов отсутствуют (доступ не состоялся).
	EventAugurAccessDenied EventType = "augur.access_denied"

	// EventServiceRegistered — Архонт зарегистрировал Service в реестре
	// `service_registry` через Operator API (`POST /v1/services`) или MCP-tool
	// `keeper.service.register` (ADR-028-паттерн RBAC-storage). `source: api`
	// или `mcp`, `archon_aid` — инициатор. Payload: `{name, git, ref,
	// created_by_aid}` — git-URL не секрет, логируется.
	EventServiceRegistered EventType = "service.registered"

	// EventServiceUpdated — Архонт заменил mutable-поля записи Service-а
	// (git/ref/refresh, replace-семантика) через Operator API
	// (`PATCH /v1/services/{name}`) или MCP-tool `keeper.service.update`.
	// `source: api` или `mcp`, `archon_aid` — инициатор. Payload: `{name, git,
	// ref}` — git-URL не секрет, логируется.
	EventServiceUpdated EventType = "service.updated"

	// EventServiceDeregistered — Архонт удалил запись Service-а из
	// `service_registry` через Operator API (`DELETE /v1/services/{name}`) или
	// MCP-tool `keeper.service.deregister`. `source: api` или `mcp`,
	// `archon_aid` — инициатор. Payload: `{name}`.
	EventServiceDeregistered EventType = "service.deregistered"

	// EventSigilKeyIntroduced — Архонт ввёл новый trust-anchor-ключ подписи Sigil
	// в реестр `sigil_signing_keys` (ADR-026(h), R3-S7) через Operator API
	// (`POST /v1/sigil/keys`) или MCP-tool `keeper.sigil.key.introduce`. `source:
	// api` или `mcp`, `archon_aid` — инициатор. Payload: `{key_id, is_primary,
	// introduced_by_aid}` — приватник (в Vault) в payload НЕ кладётся (security-
	// инвариант ADR-026(d)). key_id — стабильный SHA-256(SPKI), не секрет.
	EventSigilKeyIntroduced EventType = "sigil.key-introduced"

	// EventSigilKeyRetired — Архонт вывел trust-anchor-ключ подписи из
	// `sigil_signing_keys` (Soul забывает его при следующем SigilTrustAnchors)
	// через Operator API (`DELETE /v1/sigil/keys/{key_id}`) или MCP-tool
	// `keeper.sigil.key.retire`. `source: api` или `mcp`, `archon_aid` —
	// инициатор. Payload: `{key_id, retired_by_aid}`.
	EventSigilKeyRetired EventType = "sigil.key-retired"

	// EventSigilKeyPrimarySet — Архонт сделал active-ключ primary (новые Sigil-ы
	// подписываются им после R3-S6 reload) через Operator API
	// (`POST /v1/sigil/keys/{key_id}/primary`) или MCP-tool
	// `keeper.sigil.key.set-primary`. `source: api` или `mcp`, `archon_aid` —
	// инициатор. Payload: `{key_id, set_by_aid}`.
	EventSigilKeyPrimarySet EventType = "sigil.key-primary-set"

	// EventOmenCreated — Архонт создал Omen-запись в реестре `omens` (внешняя
	// система Augur, ADR-025 / augur.md §4.1) через Operator API
	// (`POST /v1/augur/omens`) или MCP-tool `keeper.augur.omen.create`.
	// `source: api` или `mcp`, `archon_aid` — инициатор. Payload:
	// `{name, source_type, endpoint, auth_ref, created_by_aid}` — endpoint
	// (URL внешней системы) и auth_ref (vault-ref, не сам секрет) не секрет,
	// логируются; master-credential в payload НЕ кладётся (его нет в записи —
	// только ссылка, augur.md §4.1).
	EventOmenCreated EventType = "omen.created"

	// EventOmenRevoked — Архонт удалил Omen-запись из `omens` через Operator API
	// (`DELETE /v1/augur/omens/{name}`) или MCP-tool `keeper.augur.omen.delete`.
	// `source: api` или `mcp`, `archon_aid` — инициатор. Payload: `{name}`.
	// Каскадом (ON DELETE CASCADE) убираются все связанные Rite-ы (augur.md §9).
	EventOmenRevoked EventType = "omen.revoked"

	// EventRiteCreated — Архонт создал Rite-запись (grant) в реестре `rites`
	// (ADR-025 / augur.md §4.2) через Operator API (`POST /v1/augur/rites`) или
	// MCP-tool `keeper.augur.rite.create`. `source: api` или `mcp`, `archon_aid`
	// — инициатор. Payload: `{id, omen, subject, delegate, created_by_aid}` —
	// `subject` человекочитаемая форма субъекта (`coven=<v>` / `sid=<v>`);
	// `allow`-list в payload НЕ кладётся (его форма зависит от source_type и не
	// несёт секретов, но фиксируем минимальный набор полей grant-а).
	EventRiteCreated EventType = "rite.created"

	// EventRiteRevoked — Архонт удалил Rite-запись из `rites` через Operator API
	// (`DELETE /v1/augur/rites/{id}`) или MCP-tool `keeper.augur.rite.delete`.
	// `source: api` или `mcp`, `archon_aid` — инициатор. Payload: `{id}`.
	EventRiteRevoked EventType = "rite.revoked"

	// EventVigilCreated — Архонт создал Vigil-запись в реестре `vigils`
	// (Soul-side проверка beacons-контура, ADR-030) через Operator API
	// (`POST /v1/vigils`) или MCP-tool `keeper.oracle.vigil.create`. `source:
	// api` или `mcp`, `archon_aid` — инициатор. Payload: `{name, check,
	// interval, subject, created_by_aid}` — `subject` человекочитаемая форма
	// (`coven=<v>` / `sid=<v>`); params (конфигурация проверки) в payload НЕ
	// кладётся (минимальный набор полей).
	EventVigilCreated EventType = "vigil.created"

	// EventVigilDeleted — Архонт удалил Vigil-запись из `vigils` через Operator
	// API (`DELETE /v1/vigils/{name}`) или MCP-tool `keeper.oracle.vigil.delete`.
	// `source: api` или `mcp`, `archon_aid` — инициатор. Payload: `{name}`.
	// Vigil перестаёт раздаваться хостам в VigilSnapshot; Decree-ы на него НЕ
	// каскадятся (decrees.on_beacon — text-ссылка без FK, ADR-030).
	EventVigilDeleted EventType = "vigil.deleted"

	// EventDecreeCreated — Архонт создал Decree-запись (правило reactor) в
	// реестре `decrees` (ADR-030) через Operator API (`POST /v1/decrees`) или
	// MCP-tool `keeper.oracle.decree.create`. `source: api` или `mcp`,
	// `archon_aid` — инициатор. Payload: `{name, on_beacon, incarnation,
	// action_scenario, subject, created_by_aid}` — не секрет; where-CEL и
	// action_input в payload НЕ кладутся (action_input может транзитом нести
	// vault-ref, инвариант A ADR-027).
	EventDecreeCreated EventType = "decree.created"

	// EventDecreeDeleted — Архонт удалил Decree-запись из `decrees` через
	// Operator API (`DELETE /v1/decrees/{name}`) или MCP-tool
	// `keeper.oracle.decree.delete`. `source: api` или `mcp`, `archon_aid` —
	// инициатор. Payload: `{name}`. Каскадом (ON DELETE CASCADE) чистится
	// cooldown-state в `oracle_fires` (ADR-030(a)).
	EventDecreeDeleted EventType = "decree.deleted"

	// EventOracleFired — Oracle сматчил Portent с Decree и поставил
	// named-scenario в work-queue (ADR-030(b), beacons reactor). Срабатывание
	// reactor-а — security-сигнал (недоверенный вход Soul-а вызвал действие),
	// аудируется на каждое срабатывание. `source: soul_grpc`,
	// `archon_aid: NULL` (Soul-инициированный, не оператор),
	// `correlation_id = apply_id` поставленного прогона. Payload:
	// `{decree, subject, scenario, beacon, apply_id}` — subject = авторитетный
	// SID хоста-отправителя (из mTLS peer cert); значения event.data в payload
	// НЕ кладём (могут нести произвольные данные недоверенного источника).
	EventOracleFired EventType = "oracle.fired"

	// EventIncarnationDriftChecked — оператор запустил Scry-проверку drift через
	// REST/MCP (ADR-031, on-demand-пилот). `source: api` / `mcp`, `archon_aid` —
	// инициатор; payload: `{name, scenario, apply_id, drift_summary}` —
	// `drift_summary` = `{hosts_drifted, hosts_clean, hosts_unsupported,
	// hosts_failed}` (агрегаты per-host-терминалов из DriftReport). incarnation-
	// статус `drift` после события — отдельный сигнал; здесь фиксируется именно
	// факт запуска проверки и её агрегаты. sync-под-200: audit пишется после
	// сборки DriftReport, не на приёме запроса (паритет destroy_completed —
	// событие пишется по факту, не на инициации). drift — НЕ блокирующий статус
	// (ADR-031(d)).
	EventIncarnationDriftChecked EventType = "incarnation.drift_checked"

	// EventPushApplied — оператор инициировал push-прогон Destiny по SSH через
	// Operator API (`POST /v1/push/apply`) или MCP-tool `keeper.push.apply`
	// (Variant C orchestrator, docs/keeper/push.md). `source: api` или `mcp`,
	// `archon_aid` — инициатор. Payload: `{apply_id, destiny, inventory_size,
	// ssh_provider, cleanup_stale}` — `destiny` (форма `<name>@<ref>`) и
	// `ssh_provider` (имя из keeper.yml::plugins.ssh_providers[].name) не секрет,
	// `inventory_size` — число SID-ов (сами SID-ы НЕ дублируются, лежат в
	// push_runs.inventory_sids). Пишется при приёме запроса (status: pending), до
	// старта executeAsync. Терминал — `push.completed` / `push.failed` /
	// `push.partial_failed`.
	EventPushApplied EventType = "push.applied"

	// EventPushCompleted — терминал push-прогона: все per-host SshDispatcher.SendApply
	// вернули RunResult со статусом SUCCESS. `source: api` или `mcp` (тот же, что в
	// `push.applied`), `archon_aid` — инициатор. Payload: `{apply_id, destiny,
	// inventory_size, status: "success", total, success_count, fail_count}`. Сами
	// per-host детали (sid, error) НЕ дублируются — лежат в push_runs.summary
	// (GET /v1/push/{apply_id}). status — для фильтрации в `GET /v1/audit`
	// без разбегов event_type (паттерн `task.executed`/`run.completed`).
	EventPushCompleted EventType = "push.completed"

	// EventPushFailed — терминал push-прогона: ни один хост не достиг SUCCESS
	// (все per-host SendApply провалены либо вернули RunResult не-SUCCESS), либо
	// prepare-фаза упала (inventory_load_failed / render_failed / no_live_hosts /
	// empty_plan). `source: api` или `mcp`, `archon_aid` — инициатор. Payload:
	// `{apply_id, destiny, inventory_size, status: "failed", total, success_count,
	// fail_count}` (success_count=0 для prepare-fail-а с пустыми результатами).
	// Подробности — push_runs.summary.
	EventPushFailed EventType = "push.failed"

	// EventPushPartialFailed — терминал push-прогона: смешанный исход (часть
	// хостов SUCCESS, часть failed/error_locked/error-доставки). `source: api`
	// или `mcp`, `archon_aid` — инициатор. Payload: `{apply_id, destiny,
	// inventory_size, status: "partial_failed", total, success_count, fail_count}`.
	// Подробности per-host — push_runs.summary.
	EventPushPartialFailed EventType = "push.partial_failed"

	// EventDecreeCircuitTripped — circuit-breaker Oracle авто-disable-ил Decree
	// (ADR-030(a), beacons S4): N срабатываний за окно → enabled=false. Мутация
	// Decree (симметрично `decree.created`/`decree.deleted`), поэтому имя в
	// области `decree.*`. Пишется ТОЛЬКО single-winner-ом (тот инстанс, чей
	// TripDecree выиграл, RowsAffected==1) — на каждый trip ровно одно событие.
	// `source: soul_grpc` (write-path — Soul-инициированный Portent-флоу в
	// evaluateDecree, не оператор), `archon_aid: NULL`. Payload: `{decree,
	// fire_count, window, trigger}` — `trigger` всегда `"circuit_breaker"`. БЕЗ
	// subject/beacon/event.data: trip — свойство правила (превышен порог
	// суммарно), а не отдельного хоста; недоверенный payload события не кладём.
	EventDecreeCircuitTripped EventType = "decree.circuit_tripped"

	// EventTypeErrandInvoked — оператор инициировал Errand pull-ad-hoc exec
	// через `POST /v1/souls/{sid}/exec` (ADR-033). `source: api` / `mcp`,
	// `archon_aid` — инициатор; payload: `{sid, module, errand_id,
	// timeout_seconds, dry_run}` — `input` в payload НЕ кладётся (может
	// нести vault-резолвленные секреты после CEL-render-фазы).
	EventTypeErrandInvoked EventType = "errand.invoked"

	// EventTypeErrandCompleted — Errand терминал-ил со статусом SUCCESS
	// (ADR-033). `source: soul_grpc`, `archon_aid: NULL`. Payload: `{sid,
	// module, errand_id, exit_code, duration_ms, stdout_truncated,
	// stderr_truncated}` — stdout/stderr в payload НЕ кладутся (могут быть
	// большими + маскинг идёт на выходе по общим правилам).
	EventTypeErrandCompleted EventType = "errand.completed"

	// EventTypeErrandFailed — Errand терминал-ил со статусом FAILED либо
	// MODULE_NOT_ALLOWED (ADR-033, whitelist-reject Soul-side). `source:
	// soul_grpc`, `archon_aid: NULL`. Payload: `{sid, module, errand_id,
	// exit_code, duration_ms, error_message}` — `error_message` маскированный.
	EventTypeErrandFailed EventType = "errand.failed"

	// EventTypeErrandTimedOut — Errand терминал-ил со статусом TIMED_OUT
	// (ADR-033, превышен `timeout_seconds`). `source: soul_grpc`,
	// `archon_aid: NULL`. Payload: `{sid, module, errand_id, duration_ms}`.
	EventTypeErrandTimedOut EventType = "errand.timed_out"

	// EventTypeErrandCancelled — Архонт отменил in-flight Errand через
	// `DELETE /v1/errands/{errand_id}` (ADR-033, slice E5 / post-MVP).
	// `source: api` / `mcp`, `archon_aid` — инициатор. Payload: `{errand_id,
	// sid}`.
	EventTypeErrandCancelled EventType = "errand.cancelled"

	// EventClusterDegradedSet — Toll-leader взвёл cluster:degraded флаг
	// (ADR-038): rate disconnect > threshold в sliding 60s окне. Single-winner
	// (только leader Redis-lease `cluster:toll:leader` пишет это событие).
	// `source: keeper_internal` (cluster-инициированный, не оператор),
	// `archon_aid: NULL`. Payload: `{leader_kid, rate, baseline_connected,
	// threshold, window_seconds}` — численные параметры, секретов нет.
	EventClusterDegradedSet EventType = "cluster.degraded_set"

	// EventClusterDegradedCleared — Toll-leader снял cluster:degraded флаг
	// (ADR-038): после устойчивого rate ≤ threshold в течение grace-окна
	// (asymmetric hysteresis). `source: keeper_internal`, `archon_aid: NULL`.
	// Payload: `{leader_kid, rate, baseline_connected, grace_seconds}`.
	EventClusterDegradedCleared EventType = "cluster.degraded_cleared"

	// EventSoulSshTargetUpdated — Архонт обновил per-host SSH-реквизиты push-flow
	// (ADR-032 amendment 2026-05-26, S7-1) через Operator API
	// (`PUT /v1/souls/{sid}/ssh-target`) или MCP-tool `keeper.soul.ssh-target.update`.
	// `source: api` или `mcp`, `archon_aid` — инициатор. Payload: `{sid, ssh_port,
	// ssh_user, soul_path}` — все поля cleartext (port/user/path не секрет;
	// инфраструктурные реквизиты, требуют аудита изменений симметрично coven-changes).
	EventSoulSshTargetUpdated EventType = "soul.ssh-target.updated"

	// EventPushProviderCreated — Архонт создал Push-Provider (per-provider env-payload
	// params SSH-плагина push-flow, ADR-032 amendment 2026-05-26, S7-2) через
	// Operator API (`POST /v1/push-providers`) или MCP-tool
	// `keeper.push-provider.create`. `source: api` или `mcp`, `archon_aid` —
	// инициатор. Payload: `{name, params_keys}` — `params_keys` (список ключей,
	// БЕЗ значений) фиксирует факт мутации без раскрытия секретов:
	// sensitive-значения (secret_id/token/password/private_key) обязаны быть
	// vault-refs (Service.validateSensitive), non-sensitive (vault_addr/role) —
	// не секрет, но политика «values не пишем в audit» единая для симметрии и
	// устойчивости к будущему расширению allow-list.
	EventPushProviderCreated EventType = "push-provider.created"

	// EventPushProviderUpdated — Архонт заменил params Push-Provider-а (replace-
	// семантика) через `PUT /v1/push-providers/{name}` или MCP-tool
	// `keeper.push-provider.update`. `source: api`/`mcp`, `archon_aid` — инициатор.
	// Payload: `{name, params_keys}`.
	EventPushProviderUpdated EventType = "push-provider.updated"

	// EventPushProviderDeleted — Архонт удалил запись Push-Provider-а через
	// `DELETE /v1/push-providers/{name}` или MCP-tool `keeper.push-provider.delete`.
	// `source: api`/`mcp`, `archon_aid` — инициатор. Payload: `{name}`. SshDispatcher
	// при следующем pub/sub-сигнале `push-providers:changed` re-spawn-ит плагин без
	// env-payload (либо с legacy-fallback при allow_legacy_push_providers=true).
	EventPushProviderDeleted EventType = "push-provider.deleted"

	// EventSoulSshTargetImportedFromConfig — one-shot auto-import per-host
	// SSH-реквизита push-flow из `keeper.yml::push.targets[]` в `souls.ssh_target`
	// при старте Keeper-а (ADR-032 amendment 2026-05-26, S7-4). `source:
	// config_bootstrap`, `archon_aid: NULL` (system-action). Payload: `{sid,
	// ssh_port, ssh_user, soul_path}` — все поля cleartext (инфраструктурные
	// реквизиты, не секрет; зеркало `soul.ssh-target.updated`). Идемпотентно:
	// событие пишется per-row один раз — повторный старт с уже импортированным
	// SID-ом skip-ает без события.
	EventSoulSshTargetImportedFromConfig EventType = "soul.ssh-target.imported_from_config"

	// EventPushProviderImportedFromConfig — one-shot auto-import Push-Provider
	// env-payload params из `keeper.yml::push.providers[]` в PG-таблицу
	// `push_providers` при старте Keeper-а (ADR-032 amendment 2026-05-26, S7-4).
	// `source: config_bootstrap`, `archon_aid: NULL`. Payload: `{name, params_keys}`
	// — `params_keys` (список ключей, БЕЗ значений) фиксирует факт мутации без
	// раскрытия секретов: симметрия с `push-provider.created` (Service.Create
	// audit-payload).
	EventPushProviderImportedFromConfig EventType = "push-provider.imported_from_config"

	// EventChoirCreated — Архонт создал Choir-запись (declared-топология хостов
	// внутри инкарнации, ADR-044, S-T3) через Operator API
	// (`POST /v1/incarnations/{name}/choirs`) или MCP-tool `keeper.choir.create`.
	// `source: api` или `mcp`, `archon_aid` — JWT.sub инициатора (created_by_aid
	// берётся из контекста, НЕ из тела). Payload: `{incarnation_name, choir_name,
	// min_size?, max_size?, created_by_aid}` — описание/лимиты не секрет,
	// логируются.
	EventChoirCreated EventType = "choir.created"

	// EventChoirDeleted — Архонт удалил Choir-запись через Operator API
	// (`DELETE /v1/incarnations/{name}/choirs/{choir}`) или MCP-tool
	// `keeper.choir.delete`. `source: api` или `mcp`, `archon_aid` — инициатор.
	// Payload: `{incarnation_name, choir_name}`. Каскадом (ON DELETE CASCADE)
	// сносятся все Voice-ы Choir-а.
	EventChoirDeleted EventType = "choir.deleted"

	// EventChoirVoiceAdded — Архонт добавил Voice (членство SID в Choir-е,
	// ADR-044) через Operator API
	// (`POST /v1/incarnations/{name}/choirs/{choir}/voices`) или MCP-tool
	// `keeper.choir.add-voice`. `source: api` или `mcp`, `archon_aid` —
	// инициатор (added_by_aid берётся из контекста, НЕ из тела). Payload:
	// `{incarnation_name, choir_name, sid, role?, position?, added_by_aid}` —
	// role/position omitempty (nullable declared-атрибуты).
	EventChoirVoiceAdded EventType = "choir.voice_added"

	// EventChoirVoiceRemoved — Архонт убрал Voice из Choir-а через Operator API
	// (`DELETE /v1/incarnations/{name}/choirs/{choir}/voices/{sid}`) или MCP-tool
	// `keeper.choir.remove-voice`. `source: api` или `mcp`, `archon_aid` —
	// инициатор. Payload: `{incarnation_name, choir_name, sid}`.
	EventChoirVoiceRemoved EventType = "choir.voice_removed"

	// EventScenarioRunStarted — Voyage `kind=scenario` создан/стартовал (ADR-043, S5).
	// Семантически ЗАМЕНЯЕТ `tide.started`. Пишется HTTP/MCP-handler-ом
	// `POST /v1/voyages` сразу после успешного INSERT-а pending/scheduled-row
	// (parity `errand_run.invoked` / `tide.started`: RBAC-mutating-событие).
	// `source: api` или `mcp`, `archon_aid` — JWT.sub инициатора. Payload:
	// `{voyage_id, kind, scenario_name, target (declared incarnations[]/service/
	// coven), scope_size (число резолвнутых инкарнаций), batch_size, concurrency,
	// dry_run, on_failure}` — `input` НЕ кладётся (может нести vault-резолвленные
	// секреты после CEL-render-фазы, инвариант A ADR-027). Finalize-семейство
	// (`scenario_run.completed`/`partial_failed`/`failed`/`cancelled`,
	// keeper_internal) — follow-up при подключении finalize-audit в VoyageWorker.
	// См. `scenario_run.*` блок `docs/naming-rules.md → Audit-events`.
	EventScenarioRunStarted EventType = "scenario_run.started"

	// EventCommandRunInvoked — Voyage `kind=command` создан (ADR-043, S5).
	// Семантически ЗАМЕНЯЕТ `errand_run.invoked`. Пишется HTTP/MCP-handler-ом
	// `POST /v1/voyages` сразу после успешного INSERT-а pending/scheduled-row.
	// `source: api` или `mcp`, `archon_aid` — JWT.sub инициатора. Payload:
	// `{voyage_id, kind, module, target (declared sids[]/coven/where), scope_size
	// (число резолвнутых хостов после AND-merge), batch_size, concurrency,
	// dry_run, on_failure}` — `input` НЕ кладётся (инвариант A ADR-027). Finalize-
	// семейство (`command_run.completed`/`partial_failed`/`failed`/`cancelled`,
	// keeper_internal) — follow-up при подключении finalize-audit в VoyageWorker.
	// См. `command_run.*` блок `docs/naming-rules.md → Audit-events`.
	EventCommandRunInvoked EventType = "command_run.invoked"

	// EventScenarioRunCancelled — Voyage `kind=scenario` отменён оператором
	// (ADR-043, S5): `DELETE /v1/voyages/{id}` для pending/scheduled-прогона.
	// `source: api` или `mcp`, `archon_aid` — JWT.sub инициатора. Payload:
	// `{voyage_id, kind, previous_status}` — previous_status фиксирует, из какого
	// не-running статуса прогон переведён в cancelled (running-cancel — post-MVP).
	EventScenarioRunCancelled EventType = "scenario_run.cancelled"

	// EventCommandRunCancelled — Voyage `kind=command` отменён оператором
	// (ADR-043, S5): `DELETE /v1/voyages/{id}` для pending/scheduled-прогона.
	// Семантика payload — parity [EventScenarioRunCancelled].
	EventCommandRunCancelled EventType = "command_run.cancelled"

	// EventScenarioRunLegStarted — VoyageWorker начал исполнение Leg-а
	// kind=scenario (ADR-043, finalize-audit). `source: keeper_internal`,
	// `archon_aid: NULL`, `correlation_id = voyage_id`. Эмитится ПЕРЕД fan-out-ом
	// инкарнаций Leg-а (parity `tide.surge_started`). Payload: `{voyage_id, kind,
	// leg_index, incarnations_in_leg}`. command-семейство leg-событий НЕ имеет
	// (parity `errand_run.*` — плоский fan-out без per-Leg барьера).
	EventScenarioRunLegStarted EventType = "scenario_run.leg_started"

	// EventScenarioRunLegCompleted — VoyageWorker завершил Leg kind=scenario:
	// все инкарнации Leg-а достигли терминала + агрегирован Summary-дельта
	// (parity `tide.surge_completed`). `source: keeper_internal`,
	// `archon_aid: NULL`, `correlation_id = voyage_id`. Payload: `{voyage_id, kind,
	// leg_index, terminal, total, succeeded, failed, cancelled}`.
	EventScenarioRunLegCompleted EventType = "scenario_run.leg_completed"

	// EventScenarioRunCompleted — Voyage `kind=scenario` финализирован succeeded
	// (все инкарнации success/no_match). `source: keeper_internal`,
	// `archon_aid: NULL`, `correlation_id = voyage_id`. Payload: `{voyage_id, kind,
	// total_batches, summary}` (parity `tide.completed`).
	EventScenarioRunCompleted EventType = "scenario_run.completed"

	// EventScenarioRunPartialFailed — Voyage `kind=scenario` финализирован
	// partial_failed (часть инкарнаций failed, есть хоть один успех). `source:
	// keeper_internal`, `archon_aid: NULL`, `correlation_id = voyage_id`. Payload:
	// `{voyage_id, kind, total_batches, summary, on_failure}` (parity
	// `tide.partial_failed`).
	EventScenarioRunPartialFailed EventType = "scenario_run.partial_failed"

	// EventScenarioRunFailed — Voyage `kind=scenario` финализирован failed
	// (никто не success либо fail-closed до старта инкарнаций: spawner не
	// сконфигурирован / пустой scenario_name / резолв target-а упал). `source:
	// keeper_internal`, `archon_aid: NULL`, `correlation_id = voyage_id`. Payload:
	// `{voyage_id, kind, total_batches, summary, error_code?}` — `error_code`
	// (∈ `spawner_not_configured`/`empty_scenario_name`/`target_resolve_failed`)
	// только для fail-closed-путей, при «все инкарнации failed» отсутствует.
	EventScenarioRunFailed EventType = "scenario_run.failed"

	// EventScenarioRunLeaseLost — VoyageWorker потерял lease посреди прогона
	// kind=scenario (renewal CAS вернул 0 rows — другой Keeper подобрал Voyage
	// через reclaim+claim). Orchestrator бросает работу, finalize НЕ делает
	// (parity `tide.lease_lost`). `source: keeper_internal`, `archon_aid: NULL`,
	// `correlation_id = voyage_id`. Payload: `{voyage_id, kind, kid_who_lost,
	// phase}` — `phase` ∈ `leg`/`finalize`. command-прогон при потере lease
	// отдельного события НЕ пишет (parity `errand_run.*` без lease_lost) —
	// прогон молча подберёт другой Keeper.
	EventScenarioRunLeaseLost EventType = "scenario_run.lease_lost"

	// EventCommandRunCompleted — Voyage `kind=command` финализирован succeeded
	// (все хосты success). `source: keeper_internal`, `archon_aid: NULL`,
	// `correlation_id = voyage_id`. Payload: `{voyage_id, kind, total, succeeded}`
	// (parity `errand_run.completed`).
	EventCommandRunCompleted EventType = "command_run.completed"

	// EventCommandRunPartialFailed — Voyage `kind=command` финализирован
	// partial_failed (часть хостов failed, есть хоть один успех). `source:
	// keeper_internal`, `archon_aid: NULL`, `correlation_id = voyage_id`. Payload:
	// `{voyage_id, kind, total, succeeded, failed, cancelled, on_failure}` (parity
	// `errand_run.partial_failed`).
	EventCommandRunPartialFailed EventType = "command_run.partial_failed"

	// EventCommandRunFailed — Voyage `kind=command` финализирован failed (никто
	// не success либо fail-closed до старта хостов: CommandSpawner не
	// сконфигурирован / пустой module / резолв target-а упал). `source:
	// keeper_internal`, `archon_aid: NULL`, `correlation_id = voyage_id`. Payload:
	// `{voyage_id, kind, total, succeeded, error_code?}` — `error_code`
	// (∈ `spawner_not_configured`/`empty_module`/`target_resolve_failed`) только
	// для fail-closed-путей. command-семейство leg-событий НЕ имеет (parity
	// `errand_run.*` — плоский fan-out).
	EventCommandRunFailed EventType = "command_run.failed"

	// EventVoyageReclaimed — Reaper-правило `reclaim_voyages` вернуло протухший
	// running-Voyage обратно в `pending` (claiming Keeper-инстанс мёртв либо ушёл
	// на graceful drain): row переведена `running → pending`, `claimed_by_kid →
	// NULL`, `attempt++`. Область `voyage.*` (НЕ `scenario_run.*`/`command_run.*`)
	// — kind-agnostic: SQL-реклейм не разбирает kind, событие единое для обоих
	// семейств. `source: keeper_internal`, `archon_aid: NULL`. Payload:
	// `{voyage_id, last_renewed_at, attempt_after}` (parity `tide.reclaimed`).
	EventVoyageReclaimed EventType = "voyage.reclaimed"

	// EventReconcileOrphanApplyingExecuted — Reaper-правило `reconcile_orphan_applying`
	// сняло осиротевший applying-lock инкарнации (ADR-027 amend (m)): прямой
	// (standalone, не под Voyage) scenario-run крашнувшегося Keeper-владельца
	// оставил `incarnation.status='applying'` навсегда; правило по epoch-колонкам
	// (`applying_by_kid`/`applying_since`) детектит stale-кандидата, presence-чеком
	// в Conclave подтверждает смерть владельца и снимает lock (`applying → ready`
	// через идемпотентный `ReleaseApplyingOrphan`). Область `reaper.*` (recovery-
	// действие лидера, parity `voyage.reclaimed`). `source: keeper_internal`,
	// `archon_aid: NULL`. Payload: `{incarnation, prev_kid, apply_id}` —
	// `prev_kid` = мёртвый `applying_by_kid`, `apply_id` = `applying_apply_id`.
	EventReconcileOrphanApplyingExecuted EventType = "reaper.reconcile_orphan_applying.executed"

	// EventCadenceCreated — Архонт создал Cadence-расписание (ADR-046 §8) через
	// Operator API (`POST /v1/cadences`) или MCP-tool. `source: api` или `mcp`,
	// `archon_aid` — JWT.sub инициатора (created_by_aid берётся из контекста, НЕ
	// из тела). Payload: `{cadence_id, name, schedule_kind, kind, scenario_name?,
	// module?, overlap_policy, enabled}` — `input` рецепта НЕ кладётся (инвариант A
	// ADR-027).
	EventCadenceCreated EventType = "cadence.created"

	// EventCadenceUpdated — Архонт изменил Cadence (рецепт / расписание / enabled-
	// toggle) через `PATCH /v1/cadences/{id}` или MCP-tool. `source: api`/`mcp`,
	// `archon_aid` — инициатор. Payload: `{cadence_id, name, schedule_kind, kind,
	// overlap_policy, enabled}` (поля после правки; `input` не кладётся).
	EventCadenceUpdated EventType = "cadence.updated"

	// EventCadenceDeleted — Архонт удалил Cadence через `DELETE /v1/cadences/{id}`
	// или MCP-tool. `source: api`/`mcp`, `archon_aid` — инициатор. Порождённые
	// Voyage остаются (FK `voyages.cadence_id` ON DELETE SET NULL, ADR-046 §9).
	// Payload: `{cadence_id}`.
	EventCadenceDeleted EventType = "cadence.deleted"

	// EventCadenceSpawned — Reaper-лидер на тике `spawn_due_cadence` заспавнил
	// дочерний Voyage из рецепта Cadence (ADR-046 §8): due-расписание (enabled И
	// next_run_at <= NOW()) с разрешающей overlap_policy → Insert voyages/
	// voyage_targets с back-link cadence_id, в одной spawn-tx с advance
	// next_run_at/last_run_at. Область `cadence.*` (управляющая сущность —
	// keeper-side). `source: background` (фоновое периодическое Reaper-правило,
	// parity `scry_background`; NB: ADR-046 §8 / naming-rules.md упоминают
	// `scheduler` — это значение НЕ в закрытом enum audit.Source, см.
	// observations), `archon_aid` = `created_by_aid` Cadence (спавн от имени
	// создателя, ADR-046 §7), `correlation_id` = voyage_id. Payload:
	// `{cadence_id, voyage_id, scheduled_for, scope_size}` — `scheduled_for` —
	// плановый момент (next_run_at до пересчёта); `input` рецепта НЕ кладётся
	// (инвариант A ADR-027).
	EventCadenceSpawned EventType = "cadence.spawned"

	// EventCadenceSkippedOverlap — `overlap_policy: skip` пропустила спавн из-за
	// живого предыдущего ребёнка (ADR-046 §5/§8): next_run_at наступил, но у
	// Cadence есть нетерминальный (pending/scheduled/running) порождённый Voyage →
	// спавн НЕ происходит, next_run_at всё равно пересчитывается (серия не
	// «залипает»). `source: background`, `archon_aid` = `created_by_aid`,
	// `correlation_id` = cadence_id (нет voyage_id — спавна не было). Payload:
	// `{cadence_id, scheduled_for, reason: "overlap"}`.
	EventCadenceSkippedOverlap EventType = "cadence.skipped_overlap"

	// EventHeraldDelivered — терминал УСПЕШНОЙ доставки уведомления Herald-каналу
	// (ADR-052(d), S3): claim-queue worker сделал webhook-POST, endpoint вернул
	// 2xx. at-least-once — статусы in-flight попыток живут в Redis (hot→Redis,
	// ADR-006); в audit пишется ТОЛЬКО терминал (постоянный аудируемый след).
	// `source: keeper_internal` (worker-инициированный, не оператор),
	// `archon_aid: NULL`, `correlation_id` = correlation_id события прогона
	// (voyage_id/apply_id). Payload: `{herald, tiding, event_type, attempt,
	// status_code}` — значения payload-уведомления в audit НЕ дублируются
	// (инвариант A ADR-027 — могут нести vault-резолвленные данные).
	EventHeraldDelivered EventType = "herald.delivered"

	// EventHeraldFailed — терминал ПРОВАЛА доставки уведомления (ADR-052(d), S3):
	// исчерпан retry-backoff / SSRF-guard отверг URL / endpoint недоступен /
	// non-2xx после всех попыток. `source: keeper_internal`, `archon_aid: NULL`,
	// `correlation_id` = correlation_id события прогона. Payload: `{herald,
	// tiding, event_type, attempt, error_message}` — `error_message` маскируется
	// (MaskSecrets: cause может транзитом нести vault-ref). Значения payload-
	// уведомления в audit НЕ дублируются (инвариант A ADR-027).
	EventHeraldFailed EventType = "herald.failed"

	// EventHeraldCreated / EventHeraldUpdated / EventHeraldDeleted — CRUD-семейство
	// реестра Herald-каналов (ADR-052(f), S4): оператор-инициированный CRUD через
	// POST/PUT/DELETE /v1/heralds* (и зеркальные MCP keeper.herald.*). `source`
	// = api/mcp, `archon_aid` = JWT.sub. Payload: `{name, type, url, secret_ref,
	// created_by_aid}` — `url` (для webhook не секрет) и `secret_ref` (vault-ref,
	// не сам секрет) пишутся; секрет канала в записи нет. Delete каскадом сносит
	// связанные Tiding-ы (ON DELETE CASCADE).
	EventHeraldCreated EventType = "herald.created"
	EventHeraldUpdated EventType = "herald.updated"
	EventHeraldDeleted EventType = "herald.deleted"

	// EventTidingCreated / EventTidingUpdated / EventTidingDeleted — CRUD-семейство
	// реестра Tiding-правил подписки (ADR-052(f), S4): оператор-инициированный CRUD
	// через POST/PUT/DELETE /v1/tidings* (и зеркальные MCP keeper.tiding.*).
	// `source` = api/mcp, `archon_aid` = JWT.sub. Payload: `{name, herald,
	// event_types, only_failures, only_changes, incarnation, cadence,
	// created_by_aid}` — все значения публичны (area-glob-списки / имена,
	// не секреты).
	EventTidingCreated EventType = "tiding.created"
	EventTidingUpdated EventType = "tiding.updated"
	EventTidingDeleted EventType = "tiding.deleted"

	// EventProvisioningPolicyChanged — Архонт сменил политику способов СОЗДАНИЯ
	// операторов (`provisioning_allowed_methods` в keeper_settings, ADR-058 Часть B)
	// через `PUT /v1/provisioning-policy`. `source: api`, `archon_aid` — инициатор.
	// Payload: `{allowed_methods, previous?}` — `allowed_methods` (новый список из
	// {user,ldap,oidc}) и `previous` (прежний список, если политика была задана) не
	// секрет, логируются. Мутация security-чувствительная (управляет доступом к
	// заведению операторов) — обязательно аудируется.
	EventProvisioningPolicyChanged EventType = "provisioning.policy_changed"

	// EventOperatorLogin — оператор успешно прошёл федеративную аутентификацию
	// (LDAP search-bind, ADR-058) и получил внутренний JWT через
	// `POST /auth/ldap/login`. Пишется endpoint-ом ПОСЛЕ выпуска JWT (одно
	// событие на успешный логин). `source: api`, `archon_aid` = аутентифицированный
	// AID. Payload: `{method, aid, provisioned}` — `method` ∈ `ldap` (OIDC стадия 2);
	// `provisioned` = true, если этот логин auto-провизионил нового оператора.
	// Пароль / bind-creds / группы-секреты в payload НЕ кладутся (security-гигиена).
	EventOperatorLogin EventType = "operator.login"

	// EventOperatorProvisioned — auto-provision нового Архонта при первом
	// федеративном логине (ADR-058): внешняя identity в группе из group_role_map
	// → вставка строки `operators` с `auth_method=ldap` и ролями из групп. Пишется
	// Mapper-ом при создании строки (одно событие на provision; login фиксируется
	// отдельным `operator.login`). `source: api`, `archon_aid` = новый AID. Payload:
	// `{aid, auth_method, display_name, roles, groups}` — роли/группы не секрет;
	// пароль / bind-creds в payload НЕ кладутся.
	EventOperatorProvisioned EventType = "operator.provisioned"
)
