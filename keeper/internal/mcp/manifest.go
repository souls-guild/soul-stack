package mcp

import (
	"encoding/json"
)

// toolDeclaration — MCP-spec форма tool-objectа, возвращаемая в
// `tools/list`. Поля по docs/keeper/mcp-tools.md → § Формат tool
// declaration. inputSchema / outputSchema — JSON Schema draft 2020-12
// (произвольный JSON; держим как json.RawMessage, чтобы статические
// схемы остались константами и не пересобирались на каждый list).
type toolDeclaration struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// toolStatus — метка реализации tool-а в текущем slice-е.
// Используется только tool-handler-ом для решения, вызывать ли реальный
// service или возвращать `not implemented`-error (M0.7.a — заглушки для
// 13 incarnation/soul/push/cloud-tools).
type toolStatus int

const (
	toolStatusImplemented toolStatus = iota
	toolStatusStub
)

// toolEntry — внутренняя запись каталога: declaration для tools/list +
// признак реализованности для tools/call dispatch-а.
type toolEntry struct {
	decl   toolDeclaration
	status toolStatus
}

// catalogManifest — статический манифест tool-ов (актуальное число под
// контролем детерминированного TestCatalog_TotalCount, чтобы комментарий
// не устаревал при добавлении tool-ов).
//
// Декларации синхронизируются с docs/keeper/mcp-tools.md (1:1 input/output
// schema). При расхождении — `mcp-tools.md` источник правды.
//
// `description` — короткое (1–2 предложения) объяснение для LLM-агента, что
// делает tool. Полная семантика, async-сборка, error codes — в operator-api.md
// (cross-link из mcp-tools.md).
var catalogManifest = []toolEntry{
	// --- Operator (3) — реализованы в M0.7.a ---
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.operator.create",
			Description:  "Создаёт нового Архонта (Archon) в реестре operators и выпускает для него JWT. JWT возвращается один раз — клиент обязан сохранить токен. Permission: operator.create.",
			InputSchema:  schemaOperatorCreateInput,
			OutputSchema: schemaOperatorCreateOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.operator.revoke",
			Description:  "Отзывает Архонта (ставит revoked_at=NOW). Активные JWT продолжают работать до exp. Permission: operator.revoke. Откажет с code=would-lock-out-cluster, если target — единственный активный cluster-admin.",
			InputSchema:  schemaOperatorRevokeInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.operator.issue-token",
			Description:  "Выпускает новый JWT для существующего активного Архонта с current-ролями из keeper.yml. Permission: operator.issue-token.",
			InputSchema:  schemaOperatorIssueTokenInput,
			OutputSchema: schemaOperatorIssueTokenOutput,
		},
	},

	// --- Role (6) — RBAC-CRUD, реализованы в Slice 2b ---
	//
	// 1:1 с permission (Вариант A): keeper.role.<action> ↔ role.<action>.
	// Бизнес-логика (builtin-граница, self-lockout) — в rbac.Service; tool —
	// транспорт. Все tools диспатчатся только при непустом RBACRoles (опц.
	// поле HandlerDeps); иначе call-метод вернёт «role management not configured».
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.create",
			Description:  "Создаёт RBAC-роль с набором permissions. Permission: role.create. Откажет с code=role-already-exists, если name занят, и validation-failed на битом name/permission.",
			InputSchema:  schemaRoleCreateInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.delete",
			Description:  "Удаляет RBAC-роль (каскадом permissions + membership). Permission: role.delete. Откажет с code=role-builtin для встроенной роли и would-lock-out-cluster, если снятие `*` оставит кластер без админа.",
			InputSchema:  schemaRoleDeleteInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.list",
			Description:  "Перечисление RBAC-ролей с развёрнутыми permissions и назначенными Архонтами (AID). Permission: role.list.",
			InputSchema:  schemaEmptyObject,
			OutputSchema: schemaRoleListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.update",
			Description:  "Заменяет набор permissions роли (replace-семантика). Permission: role.update. Откажет с code=role-builtin для встроенной роли и would-lock-out-cluster при снятии последнего `*`.",
			InputSchema:  schemaRoleUpdateInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.grant-operator",
			Description:  "Привязывает Архонта (AID) к роли. Идемпотентно. Permission: role.grant-operator. Откажет с code=not-found, если роль или AID не существуют.",
			InputSchema:  schemaRoleGrantOperatorInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.revoke-operator",
			Description:  "Снимает привязку Архонта (AID) от роли. Permission: role.revoke-operator. Откажет с code=would-lock-out-cluster, если снимается последний админ с `*`.",
			InputSchema:  schemaRoleRevokeOperatorInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Synod (8) — ADR-049 ---
	//
	// Группы архонов (Архон → Synod → Роли). 1:1 keeper.synod.<action> ↔
	// permission synod.<action>. Бизнес-логика (builtin-граница, least-privilege
	// subset на add-operator/grant-role, self-lockout на delete/remove-operator/
	// revoke-role) — в rbac.Service; tool — транспорт. Диспатчатся только при
	// непустом RBACRoles.
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.create",
			Description:  "Создаёт Synod-группу (бандлит роли для набора архонов). Permission: synod.create. Откажет с code=synod-already-exists, если name занят, и validation-failed на битом name.",
			InputSchema:  schemaSynodCreateInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.update",
			Description:  "Меняет ТОЛЬКО описание Synod-группы (name (PK) immutable). Permission: synod.update. builtin РАЗРЕШЁН (description косметика, без subset/self-lockout). Откажет с code=synod-not-found, если группы нет.",
			InputSchema:  schemaSynodUpdateInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.delete",
			Description:  "Удаляет Synod-группу (каскадом membership + bundle). Permission: synod.delete. Откажет с code=synod-builtin для встроенной группы и would-lock-out-cluster, если исчезновение группы оставит кластер без админа с `*`.",
			InputSchema:  schemaSynodDeleteInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.list",
			Description:  "Перечисление Synod-групп с развёрнутыми ролями (bundle) и членами (AID). Permission: synod.list.",
			InputSchema:  schemaEmptyObject,
			OutputSchema: schemaSynodListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.add-operator",
			Description:  "Добавляет архона (AID) в Synod-группу. Идемпотентно. Permission: synod.add-operator. Под least-privilege subset: откажет с code=forbidden, если инициатор не держит права bundle группы. Откажет с code=not-found на несуществующую группу/AID.",
			InputSchema:  schemaSynodAddOperatorInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.remove-operator",
			Description:  "Убирает архона (AID) из Synod-группы. Permission: synod.remove-operator. Откажет с code=would-lock-out-cluster, если снятие осиротит последнего админа с `*`.",
			InputSchema:  schemaSynodRemoveOperatorInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.grant-role",
			Description:  "Добавляет роль в bundle Synod-группы (выдаёт её всем членам). Идемпотентно. Permission: synod.grant-role. Под least-privilege subset: откажет с code=forbidden, если инициатор не держит права роли. Откажет с code=not-found на несуществующую группу/роль.",
			InputSchema:  schemaSynodGrantRoleInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.revoke-role",
			Description:  "Снимает роль из bundle Synod-группы (у всех членов). Permission: synod.revoke-role. Откажет с code=would-lock-out-cluster, если снятие осиротит последнего админа с `*`.",
			InputSchema:  schemaSynodRevokeRoleInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Incarnation (10) ---
	//
	// Все 10 tools (create/run/get/list/history/unlock/rerun-create/upgrade/destroy/check-drift)
	// Implemented: dispatch-ветки заведены, тела — паритет REST
	// IncarnationHandler. destroy подключён в S-D4 (DELETE /v1/incarnations/{name}).
	// check-drift подключён в ADR-031 Slice B (Scry on-demand-пилот).
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.create",
			Description:  "Создаёт новый Incarnation: запускает scenario 'create' указанного Service. Асинхронная операция — возвращает _apply_id. Permission: incarnation.create.",
			InputSchema:  schemaIncarnationCreateInput,
			OutputSchema: schemaApplyIDOutputWithIncarnation,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.run",
			Description:  "Запускает произвольный сценарий над existing Incarnation. Асинхронная операция — возвращает _apply_id. Permission: incarnation.run.",
			InputSchema:  schemaIncarnationRunInput,
			OutputSchema: schemaIncarnationRunOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.get",
			Description:  "Читает spec + state + status Incarnation по имени. Permission: incarnation.get.",
			InputSchema:  schemaIncarnationGetInput,
			OutputSchema: schemaIncarnationGetOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.list",
			Description:  "Перечисление Incarnation-ов с фильтрацией по service/status и pagination. Permission: incarnation.list.",
			InputSchema:  schemaIncarnationListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.history",
			Description:  "Возвращает журнал state_history для Incarnation с pagination. Используется для опроса async-операций (_apply_id появится в history после успешного commit-а). Permission: incarnation.history.",
			InputSchema:  schemaIncarnationHistoryInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.unlock",
			Description:  "Снимает состояние error_locked с Incarnation. Permission: incarnation.unlock.",
			InputSchema:  schemaIncarnationUnlockInput,
			OutputSchema: schemaIncarnationUnlockOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.rerun-create",
			Description:  "Снимает error_locked и тем же действием перезапускает scenario 'create' (rerun bootstrap-а). Только из error_locked. Асинхронная операция — возвращает _apply_id. Permission: incarnation.create-rerun.",
			InputSchema:  schemaIncarnationRerunCreateInput,
			OutputSchema: schemaApplyIDOutputWithIncarnation,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.upgrade",
			Description:  "Переводит Incarnation на новую state_schema_version + сменяет service_version. Асинхронная операция — возвращает _apply_id. Permission: incarnation.upgrade.",
			InputSchema:  schemaIncarnationUpgradeInput,
			OutputSchema: schemaApplyIDOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.destroy",
			Description:  "Сносит Incarnation. allow_destroy=false — destroy через teardown-сценарий 'destroy'; allow_destroy=true — снос без teardown (force). Асинхронная операция — возвращает _apply_id. Permission: incarnation.destroy.",
			InputSchema:  schemaIncarnationDestroyInput,
			OutputSchema: schemaApplyIDOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.check-drift",
			Description:  "Scry-проверка drift (ADR-031): Keeper рендерит scenario 'converge' и шлёт всем хостам ApplyRequest{dry_run:true} (Soul зовёт mod.Plan вместо mod.Apply), собирает per-host per-task changed и возвращает DriftReport. Sync. input — optional override converge-параметров; auto-from-state по конвенции имени. Permission: incarnation.check-drift. Откажет с code=validation-failed, если converge отсутствует в service-snapshot-е или drift-input не резолвится.",
			InputSchema:  schemaIncarnationCheckDriftInput,
			OutputSchema: schemaIncarnationCheckDriftOutput,
		},
	},

	// --- Soul (5) — create + issue-token + coven-assign + ssh-target.update
	// implemented (паритет REST POST /v1/souls + issue-token + coven +
	// ssh-target); list остаётся stub (ждёт M2). ---
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.create",
			Description:  "Регистрирует Soul в реестре souls (status: pending) и для transport=agent выпускает первый bootstrap-токен (souls-row + token атомарно). Токен возвращается один раз — клиент обязан сохранить. Permission: soul.create.",
			InputSchema:  schemaSoulCreateInput,
			OutputSchema: schemaSoulCreateOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.issue-token",
			Description:  "Повторно выпускает bootstrap-токен для существующей Soul (transport=agent). Токен возвращается один раз. force=true истекает активный токен и выписывает новый. Permission: soul.issue-token. Откажет с code=bootstrap-token-active, если активный токен есть, а force не передан.",
			InputSchema:  schemaSoulIssueTokenInput,
			OutputSchema: schemaSoulIssueTokenOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.coven-assign",
			Description:  "Массово добавляет (mode=append) / снимает (mode=remove) ОДНУ Coven-метку либо ЗАМЕНЯЕТ (mode=replace) набор Coven-меток на хостах под selector (all/sids/coven/incarnation/status) ∩ coven-scope оператора. append/remove требует поле 'label', replace требует 'labels[]' (может быть пустым = снять все). Coven — холодная PG-метка. Permission: soul.coven-assign. dry_run=true возвращает matched без UPDATE. Откажет с code=validation-failed на пустом селекторе или метке/каждой метке набора вне coven-scope оператора.",
			InputSchema:  schemaSoulCovenAssignInput,
			OutputSchema: schemaSoulCovenAssignOutput,
		},
	},
	{
		status: toolStatusStub,
		decl: toolDeclaration{
			Name:         "keeper.soul.list",
			Description:  "Перечисление Souls с фильтрацией по coven/status/transport и pagination. Permission: soul.list.",
			InputSchema:  schemaSoulListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.ssh-target.update",
			Description:  "Обновляет per-host SSH-реквизиты push-flow (souls.ssh_target jsonb, ADR-032 amendment 2026-05-26, S7-1): ssh_port/ssh_user/soul_path. Source-of-truth для PGFallbackTargetResolver; keeper.yml::push.targets[] — legacy fallback под флагом push.allow_legacy_push_targets. Permission: soul.ssh-target-update; selector host=<sid>. Откажет с code=not-found, если SID отсутствует в реестре souls.",
			InputSchema:  schemaSoulSshTargetUpdateInput,
			OutputSchema: schemaSoulSshTargetUpdateOutput,
		},
	},

	// --- Plugin (3) — Sigil allow-list, реализованы в S4b ---
	//
	// 1:1 с permission (keeper.plugin.<action> ↔ plugin.<action>) и REST
	// POST/GET/DELETE /v1/plugins/sigils*. Бизнес-логика (чтение слота кеша,
	// подпись, CRUD реестра) — в sigil.Service; tool — транспорт. Все три
	// диспатчатся только при непустом SigilSvc (опц. поле HandlerDeps); иначе
	// call-метод вернёт «sigil is not configured».
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.plugin.allow",
			Description:  "Допускает плагин (namespace, name) под operator-asserted меткой ref в allow-list plugin_sigils: Keeper читает текущий бинарь из single-slot кеша, считает sha256, подписывает и вставляет запись. Permission: plugin.allow. Откажет с code=plugin-not-in-cache, если плагина нет в кеше, и sigil-already-active, если активный допуск на (ns, name, ref) уже есть.",
			InputSchema:  schemaPluginAllowInput,
			OutputSchema: schemaPluginAllowOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.plugin.revoke",
			Description:  "Отзывает активный допуск (namespace, name, ref) из allow-list plugin_sigils (бинарь перестаёт проходить Sigil-верификацию). Permission: plugin.revoke. Откажет с code=sigil-not-found, если активной записи нет.",
			InputSchema:  schemaPluginRevokeInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.plugin.list",
			Description:  "Перечисление активных записей allow-list-а plugin_sigils (без signature/manifest), новые первыми. Permission: plugin.list.",
			InputSchema:  schemaEmptyObject,
			OutputSchema: schemaPluginListOutput,
		},
	},

	// --- Sigil-key (4) — ротация ключей подписи Sigil, реализованы в R3-S7 ---
	//
	// keeper.sigil.key.<verb> ↔ permission sigil.key-<verb> ↔ REST /v1/sigil/keys*.
	// Бизнес-логика (key-gen + Vault-write + CRUD реестра + publish anchors-changed)
	// — в sigil.KeyService; tool — транспорт. Все четыре диспатчатся только при
	// непустом SigilKeySvc (опц. поле HandlerDeps); иначе call-метод вернёт «sigil
	// is not configured». БЕЗОПАСНОСТЬ: приватник НИКОГДА не в output.
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.sigil.key.introduce",
			Description:  "Вводит новый trust-anchor-ключ подписи Sigil: Keeper генерирует ed25519-пару, пишет приватник в Vault KV и вставляет публичную часть (SPKI) в реестр sigil_signing_keys как active. Возвращает key_id + pubkey_pem (НЕ приватник). make_primary=true делает ключ primary (новые Sigil-ы подписываются им). Permission: sigil.key-introduce. После ввода кластер re-load-ит набор якорей (anchors-changed). Откажет с code=sigil-key-concurrent-change при гонке primary.",
			InputSchema:  schemaSigilKeyIntroduceInput,
			OutputSchema: schemaSigilKeyIntroduceOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.sigil.key.list",
			Description:  "Перечисление active trust-anchor-ключей подписи Sigil (primary первым). Без vault_ref. Permission: sigil.key-list.",
			InputSchema:  schemaEmptyObject,
			OutputSchema: schemaSigilKeyListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.sigil.key.set-primary",
			Description:  "Делает active-ключ primary (новые Sigil-ы подписываются им после cluster reload). Permission: sigil.key-set-primary. Откажет с code=sigil-key-not-found, если ключа нет, и sigil-key-concurrent-change при гонке primary либо если ключ retired.",
			InputSchema:  schemaSigilKeyIDInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.sigil.key.retire",
			Description:  "Выводит trust-anchor-ключ подписи из набора (Soul забывает его при следующем SigilTrustAnchors). Permission: sigil.key-retire. Откажет с code=sigil-key-not-found (active-записи нет), sigil-key-last-active (последний active) и sigil-key-primary (primary напрямую — сперва set-primary другому).",
			InputSchema:  schemaSigilKeyIDInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Service (4) — реестр Service-ов, реализованы в S3 ---
	//
	// 1:1 с permission (keeper.service.<action> ↔ service.<action>) и REST
	// POST/GET/PATCH/DELETE /v1/services*. Бизнес-логика (валидация name/git/ref/
	// refresh, invalidate-хук) — в serviceregistry.Service; tool — транспорт.
	// Все четыре диспатчатся только при непустом ServiceSvc (опц. поле
	// HandlerDeps); иначе call-метод вернёт «service registry is not configured».
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.service.register",
			Description:  "Регистрирует Service в реестре service_registry (git-источник service-репо + ref + опц. авто-refresh). Permission: service.register. Откажет с code=service-already-exists, если name занят, validation-failed на битом name/git/ref/refresh и not-found, если AID создателя отсутствует в реестре operators.",
			InputSchema:  schemaServiceRegisterInput,
			OutputSchema: schemaServiceView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.service.update",
			Description:  "Заменяет mutable-поля записи Service-а (git/ref/refresh, replace-семантика); name — ключ, не меняется. Permission: service.update. Откажет с code=not-found, если записи нет, и validation-failed на битом git/ref/refresh.",
			InputSchema:  schemaServiceUpdateInput,
			OutputSchema: schemaServiceView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.service.list",
			Description:  "Перечисление зарегистрированных Service-ов (sort name ASC). Permission: service.list.",
			InputSchema:  schemaEmptyObject,
			OutputSchema: schemaServiceListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.service.deregister",
			Description:  "Удаляет запись Service-а из реестра service_registry по имени. Permission: service.deregister. Откажет с code=not-found, если записи нет.",
			InputSchema:  schemaServiceDeregisterInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Augur (6) — реестры Omen / Rite, реализованы (ADR-025) ---
	//
	// 4-сегментный tool-name keeper.augur.<resource>.<action> ↔ 2-сегментная
	// permission <resource>.<action> (omen.create / rite.list / …). Бизнес-логика
	// (валидация name/source_type/auth_ref, XOR-субъект, allow-shape, token-поля)
	// — в augur.Service; tool — транспорт. Все шесть диспатчатся только при
	// непустом AugurSvc (опц. поле HandlerDeps); иначе call-метод вернёт «augur
	// registry is not configured».
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.omen.create",
			Description:  "Создаёт Omen в реестре omens (внешняя система Augur: vault / prometheus / elk + endpoint + auth_ref — vault-ref на master-cred, не сам секрет). Permission: omen.create. Откажет с code=omen-already-exists, если name занят, validation-failed на битом name/source_type/endpoint/auth_ref.",
			InputSchema:  schemaOmenCreateInput,
			OutputSchema: schemaOmenView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.omen.list",
			Description:  "Перечисление Omen-ов реестра (sort created_at DESC, name ASC; опц. offset/limit). Permission: omen.list.",
			InputSchema:  schemaOmenListInput,
			OutputSchema: schemaOmenListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.omen.delete",
			Description:  "Удаляет Omen по имени; каскадно убирает связанные Rite-ы (ON DELETE CASCADE). Permission: omen.delete. Откажет с code=not-found, если записи нет.",
			InputSchema:  schemaOmenDeleteInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.rite.create",
			Description:  "Создаёт Rite (grant) в реестре rites: субъект (coven XOR sid) × omen → allow-list + delegate + опц. token_ttl/token_num_uses (только для vault-delegate). Permission: rite.create. Откажет с code=not-found, если Omen не существует, и validation-failed на нарушении XOR / битом allow / token-полях.",
			InputSchema:  schemaRiteCreateInput,
			OutputSchema: schemaRiteView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.rite.list",
			Description:  "Перечисление Rite-ов одного Omen-а (фильтр omen обязателен; sort created_at DESC, id ASC). Permission: rite.list.",
			InputSchema:  schemaRiteListInput,
			OutputSchema: schemaRiteListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.rite.delete",
			Description:  "Удаляет Rite по суррогатному id. Permission: rite.delete. Откажет с code=not-found, если записи нет.",
			InputSchema:  schemaRiteDeleteInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Oracle (6) — реестры Vigil / Decree, реализованы (ADR-030 beacons) ---
	//
	// 4-сегментный tool-name keeper.oracle.<resource>.<action> ↔ 2-сегментная
	// permission <resource>.<action> (vigil.create / decree.list / …). Бизнес-
	// логика (валидация name/interval/check/субъект для Vigil; name/on_beacon/
	// incarnation/scenario/субъект/where-CEL для Decree) — в oracle.Service; tool
	// — транспорт. Все шесть диспатчатся только при непустом OracleSvc (опц. поле
	// HandlerDeps); иначе call-метод вернёт «oracle registry is not configured».
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.vigil.create",
			Description:  "Создаёт Vigil в реестре vigils (Soul-side проверка beacons: check — адрес core.beacon + interval + субъект coven XOR sid). Read-only по конструкции (наблюдает, не мутирует хост). Permission: vigil.create. Откажет с code=vigil-already-exists, если name занят, validation-failed на битом name/interval/check/субъекте.",
			InputSchema:  schemaVigilCreateInput,
			OutputSchema: schemaVigilView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.vigil.list",
			Description:  "Перечисление Vigil-ов реестра (sort created_at DESC, name ASC; опц. offset/limit). Permission: vigil.list.",
			InputSchema:  schemaOraclePaginatedInput,
			OutputSchema: schemaVigilListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.vigil.delete",
			Description:  "Удаляет Vigil по имени (перестаёт раздаваться хостам в VigilSnapshot; Decree-ы НЕ каскадятся). Permission: vigil.delete. Откажет с code=not-found, если записи нет.",
			InputSchema:  schemaOracleNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.decree.create",
			Description:  "Создаёт Decree (правило reactor) в реестре decrees: on_beacon (Vigil) × субъект (coven XOR sid) × incarnation_name → action_scenario (named, whitelist) + опц. where-CEL предикат над event.data + cooldown. Default-deny. Permission: decree.create. Откажет с code=decree-already-exists, если name занят, validation-failed на битом name/on_beacon/incarnation_name/action_scenario/субъекте/where-CEL/cooldown.",
			InputSchema:  schemaDecreeCreateInput,
			OutputSchema: schemaDecreeView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.decree.list",
			Description:  "Перечисление Decree-ов реестра (sort created_at DESC, name ASC; опц. offset/limit). Permission: decree.list.",
			InputSchema:  schemaOraclePaginatedInput,
			OutputSchema: schemaDecreeListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.decree.delete",
			Description:  "Удаляет Decree по имени; каскадно чистит cooldown-state (oracle_fires, ON DELETE CASCADE). Permission: decree.delete. Откажет с code=not-found, если записи нет.",
			InputSchema:  schemaOracleNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Errand (4) — pull-ad-hoc exec ADR-033, slice E4 + cancel slice E5 ---
	//
	// 1:1 с REST POST /v1/souls/{sid}/exec + GET /v1/errands{,/{errand_id}} +
	// DELETE /v1/errands/{errand_id} и permission (errand.run — селектор
	// host=<sid>; errand.list — NoSelector; errand.cancel — NoSelector).
	// Бизнес-логика (validate, INSERT, send/wait, mask+cap, async-escalation,
	// cancel-signal, audit) — в errand.Dispatcher / Store; tool — транспорт. Все
	// диспатчатся только при непустых ErrandDispatcher/ErrandStore (опц. поля
	// HandlerDeps); иначе call-метод вернёт «errand orchestrator is not configured».
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.errand.run",
			Description:  "Запускает одиночный модуль на Soul через mTLS EventStream (pull-ad-hoc exec, ADR-033). Возвращает результат sync (status terminal) либо async=true со status=running, если server-cap превышен — дальше poll keeper.errand.get. Whitelist модулей и cap stdout/stderr (64 KiB) применяет Soul-side errand-runner. Permission: errand.run; селектор host=<sid>. Откажет с code=not-found, если Soul не подключён к кластеру; validation-failed на пустом sid/module и timeout_seconds вне [1,300].",
			InputSchema:  schemaErrandRunInput,
			OutputSchema: schemaErrandRunOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.errand.list",
			Description:  "Перечисление Errand-ов с фильтрами sid/status/started_after и pagination (sort started_at DESC). Read-only. Permission: errand.list.",
			InputSchema:  schemaErrandListInput,
			OutputSchema: schemaErrandListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.errand.get",
			Description:  "Читает текущее состояние Errand-а по ULID. Для running-строки возвращает status=running без stdout/exit_code (poll). Permission: errand.list. Откажет с code=not-found, если errand_id не существует.",
			InputSchema:  schemaErrandGetInput,
			OutputSchema: schemaErrandRow,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.errand.cancel",
			Description:  "Отменяет in-flight Errand (slice E5, ADR-033). Best-effort: Keeper отправляет CancelErrand Soul-у, Soul-side errandrunner отменяет ctx → возвращает ErrandResult{CANCELLED}. 204-эквивалент при успехе. Permission: errand.cancel. Откажет с code=not-found (errand_id не существует / Soul не подключён) либо code=errand-not-cancellable (Errand уже в терминальном статусе).",
			InputSchema:  schemaErrandCancelInput,
			OutputSchema: schemaErrandCancelOutput,
		},
	},

	// --- Voyage (4) — унифицированный батчевый прогон (ADR-043, S5). Диспатчатся
	// только при непустых Voyage-deps (VoyageDB + резолверы); иначе call-метод
	// вернёт «voyage orchestrator is not configured». RBAC-by-kind (scenario→
	// incarnation.run / command→errand.run) делает сам handler.
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.voyage.start",
			Description:  "Создаёт Voyage — унифицированный батчевый прогон (ADR-043). kind=scenario: применить named scenario к набору ИНКАРНАЦИЙ (target incarnations[] ∪ service/coven; per-incarnation state-commit, B1). kind=command: выполнить whitelisted-модуль на наборе ХОСТОВ (target sids/coven/where, AND-merge; state не трогается). Batch (Leg) = N единиц (batch_size); on_failure abort|continue; schedule_at → отложенный старт. Async: 202 + voyage_id; прогресс — polling keeper.voyage.get. RBAC-by-kind (security-критичный): scenario→incarnation.run, command→errand.run.",
			InputSchema:  schemaVoyageStartInput,
			OutputSchema: schemaVoyageStartOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.voyage.get",
			Description:  "Читает snapshot Voyage по ULID (detail + summary). Permission: incarnation.history. Откажет с code=not-found, если voyage_id не существует.",
			InputSchema:  schemaVoyageGetInput,
			OutputSchema: schemaVoyageView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.voyage.list",
			Description:  "Перечисление Voyage-прогонов с фильтрами kind/status (multi-value) и pagination (sort created_at DESC). Permission: incarnation.history.",
			InputSchema:  schemaVoyageListInput,
			OutputSchema: schemaVoyageListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.voyage.cancel",
			Description:  "Отменяет Voyage (pending/scheduled → cancelled, ADR-043 S5). Running-abort — post-MVP (code=errand-not-cancellable). RBAC-by-kind как у start. Откажет с code=not-found, если voyage_id не существует, либо code=errand-not-cancellable, если Voyage running/терминал.",
			InputSchema:  schemaVoyageGetInput,
			OutputSchema: schemaVoyageCancelOutput,
		},
	},

	// --- Push (2) — keeper.push.apply реализован (Variant C orchestrator,
	// docs/keeper/push.md); keeper.push.cleanup остаётся stub (отдельный slice).
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push.apply",
			Description:  "Push-прогон Destiny по SSH на инвентарь хостов (Variant C orchestrator). Асинхронная операция — возвращает _apply_id. Permission: push.apply.",
			InputSchema:  schemaPushApplyInput,
			OutputSchema: schemaApplyIDOutput,
		},
	},
	{
		status: toolStatusStub,
		decl: toolDeclaration{
			Name:         "keeper.push.cleanup",
			Description:  "Чистка /var/lib/soul-stack/ на хостах из инвентаря. Асинхронная операция — возвращает _apply_id. Permission: push.cleanup.",
			InputSchema:  schemaPushCleanupInput,
			OutputSchema: schemaApplyIDOutput,
		},
	},

	// --- Cloud (2) — stubs в M0.7.a (ждёт CloudDriver-инфраструктуры) ---
	{
		status: toolStatusStub,
		decl: toolDeclaration{
			Name:         "keeper.provider.create",
			Description:  "Создаёт Cloud Provider в реестре providers. Permission: provider.create.",
			InputSchema:  schemaProviderCreateInput,
			OutputSchema: schemaProviderCreateOutput,
		},
	},
	{
		status: toolStatusStub,
		decl: toolDeclaration{
			Name:         "keeper.profile.create",
			Description:  "Создаёт Cloud Profile (VM-параметры) в реестре profiles. Permission: profile.create.",
			InputSchema:  schemaProfileCreateInput,
			OutputSchema: schemaProfileCreateOutput,
		},
	},

	// --- Push-Provider (5) — CRUD реестра, реализованы в S7-2 ---
	//
	// keeper.push-provider.<verb> ↔ permission push-provider.<verb> ↔ REST
	// POST/GET/PUT/DELETE /v1/push-providers*. Бизнес-логика (валидация
	// sensitive-params как vault-refs, Redis invalidate-publish) — в
	// pushprovider.Service; tool — транспорт. Все пять диспатчатся только при
	// непустом PushProviderSvc (опц. поле HandlerDeps); иначе call-метод
	// возвращает internal-error «push-provider registry is not configured»
	// (паттерн ServiceSvc/AugurSvc).
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push-provider.create",
			Description:  "Создаёт Push-Provider в реестре push_providers (per-provider env-payload params SSH-плагина push-flow, ADR-032 amendment 2026-05-26, S7-2). Sensitive params (secret_id/token/password/private_key) ОБЯЗАНЫ быть vault-refs (vault:<path>). После commit-а cluster-wide invalidate через Redis pub/sub push-providers:changed → SshDispatcher re-spawn-ит плагин на ближайшем RPC. Permission: push-provider.create.",
			InputSchema:  schemaPushProviderCreateInput,
			OutputSchema: schemaPushProviderView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push-provider.update",
			Description:  "Заменяет params Push-Provider-а (replace-семантика; name — ключ, не меняется). Sensitive-инвариант тот же. Permission: push-provider.update. Откажет с code=not-found, если записи нет.",
			InputSchema:  schemaPushProviderUpdateInput,
			OutputSchema: schemaPushProviderView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push-provider.delete",
			Description:  "Удаляет запись Push-Provider-а. Permission: push-provider.delete. Откажет с code=not-found, если записи нет.",
			InputSchema:  schemaPushProviderByNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push-provider.list",
			Description:  "Перечисление Push-Provider-ов (sort updated_at DESC). Permission: push-provider.list.",
			InputSchema:  schemaPushProviderListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push-provider.read",
			Description:  "Читает одну запись Push-Provider-а по имени. Permission: push-provider.read.",
			InputSchema:  schemaPushProviderByNameInput,
			OutputSchema: schemaPushProviderView,
		},
	},

	// --- Herald (5) — CRUD реестра каналов уведомлений (ADR-052, S4) ---
	//
	// keeper.herald.<verb> ↔ permission herald.<verb> ↔ REST POST/GET/PUT/DELETE
	// /v1/heralds*. Бизнес-логика (валидация config/secret_ref + SSRF-контур +
	// инвалидация dispatcher-кэша) — в herald.Service; tool — транспорт. Все пять
	// диспатчатся только при непустом HeraldSvc (опц. поле HandlerDeps); иначе
	// call-метод возвращает internal-error «herald registry is not configured».
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.herald.create",
			Description:  "Создаёт Herald-канал доставки уведомлений (ADR-052; webhook в MVP: config.url + опц. headers; SSRF-контур https-only + deny приватных IP взведён по умолчанию, снимается config.http_allowed/allow_private). secret_ref (опц.) — vault-ref на signing-token (подпись webhook X-SoulStack-Signature: sha256=<hex>). Permission: herald.create. Откажет с code=herald-already-exists, если name занят, validation-failed на битом name/type/config/secret_ref.",
			InputSchema:  schemaHeraldCreateInput,
			OutputSchema: schemaHeraldView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.herald.update",
			Description:  "Заменяет mutable-поля Herald-канала (replace-семантика; name — ключ, не меняется). SSRF-инвариант тот же, что у create. Permission: herald.update. Откажет с code=not-found, если записи нет.",
			InputSchema:  schemaHeraldUpdateInput,
			OutputSchema: schemaHeraldView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.herald.delete",
			Description:  "Удаляет Herald-канал; каскадно сносит связанные Tiding-подписки (ON DELETE CASCADE). Permission: herald.delete. Откажет с code=not-found, если записи нет.",
			InputSchema:  schemaHeraldByNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.herald.list",
			Description:  "Перечисление Herald-каналов (sort updated_at DESC, name ASC; опц. offset/limit). Permission: herald.list.",
			InputSchema:  schemaHeraldListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.herald.read",
			Description:  "Читает один Herald-канал по имени. Permission: herald.read.",
			InputSchema:  schemaHeraldByNameInput,
			OutputSchema: schemaHeraldView,
		},
	},

	// --- Tiding (5) — CRUD реестра правил подписки (ADR-052, S4) ---
	//
	// keeper.tiding.<verb> ↔ permission tiding.<verb> ↔ REST POST/GET/PUT/DELETE
	// /v1/tidings*. event_types — area-glob в scope прогонов; herald — FK на
	// существующий Herald. Те же HeraldSvc / nil-guard, что herald-tools.
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.tiding.create",
			Description:  "Создаёт Tiding-правило подписки (ADR-052): на какие event_types (area-glob scenario_run.* в scope прогонов: scenario_run/command_run/voyage/cadence + incarnation.drift_checked) реагировать → каким Herald-ом доставлять. Фильтры only_failures/only_changes, опц. селекторы incarnation/cadence. Permission: tiding.create. Откажет с code=tiding-already-exists (name занят), not-found (herald не существует), validation-failed (битый name/event_types).",
			InputSchema:  schemaTidingCreateInput,
			OutputSchema: schemaTidingView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.tiding.update",
			Description:  "Заменяет mutable-поля Tiding-правила (replace-семантика; name — ключ). Permission: tiding.update. Откажет с code=not-found, если правила нет или herald по FK не существует.",
			InputSchema:  schemaTidingUpdateInput,
			OutputSchema: schemaTidingView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.tiding.delete",
			Description:  "Удаляет Tiding-правило по имени. Permission: tiding.delete. Откажет с code=not-found, если записи нет.",
			InputSchema:  schemaTidingByNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.tiding.list",
			Description:  "Перечисление Tiding-правил (sort updated_at DESC, name ASC; опц. offset/limit). По умолчанию скрывает разовые (ephemeral) правила; include_ephemeral=true отдаёт все. Permission: tiding.list.",
			InputSchema:  schemaTidingListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.tiding.read",
			Description:  "Читает одно Tiding-правило по имени. Permission: tiding.read.",
			InputSchema:  schemaTidingByNameInput,
			OutputSchema: schemaTidingView,
		},
	},
}

// toolByName — линейный поиск по каталогу. Возвращает ok=false если name
// не зарегистрировано.
func toolByName(name string) (toolEntry, bool) {
	for _, e := range catalogManifest {
		if e.decl.Name == name {
			return e, true
		}
	}
	return toolEntry{}, false
}

// listAllTools — упорядоченный snapshot всех tool-declarations для
// `tools/list`. Порядок — declaration-order из catalogManifest (operator
// → role → incarnation → soul → plugin → service → augur → oracle → push → cloud),
// соответствует docs/keeper/mcp-tools.md.
func listAllTools() []toolDeclaration {
	out := make([]toolDeclaration, len(catalogManifest))
	for i, e := range catalogManifest {
		out[i] = e.decl
	}
	return out
}

// --- JSON Schema-литералы для tool-declarations ---
//
// Каждая схема — JSON Schema draft 2020-12, additionalProperties=false,
// required-поля заданы явно. Хранятся как json.RawMessage, чтобы пакет
// мог отдавать их в tools/list без re-marshal-а на каждый запрос.

var (
	schemaEmptyObject = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{}}`)

	schemaApplyIDOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["_apply_id"],
"properties":{
"_apply_id":{"type":"string","description":"ULID запуска."}}}`)

	schemaApplyIDOutputWithIncarnation = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["incarnation"],
"properties":{
"_apply_id":{"type":"string","description":"ULID запуска scenario create. Отсутствует, если lifecycle.auto_create=false — инкарнация создана в ready без прогона."},
"incarnation":{"type":"string"}}}`)

	schemaPaginatedListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["items","offset","limit","total"],
"properties":{
"items":{"type":"array"},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000},
"total":{"type":"integer","minimum":0}}}`)

	schemaOperatorCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["aid","display_name"],
"properties":{
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$","description":"AID нового Архонта."},
"display_name":{"type":"string","description":"Человекочитаемое имя."}}}`)

	schemaOperatorCreateOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["aid","display_name","created_at","created_by_aid","jwt","expires_at"],
"properties":{
"aid":{"type":"string"},
"display_name":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":"string"},
"jwt":{"type":"string","description":"Выпускается один раз; клиент обязан сохранить."},
"expires_at":{"type":"string","format":"date-time"}}}`)

	schemaOperatorRevokeInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["aid"],
"properties":{
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"},
"reason":{"type":"string","description":"Свободный текст для audit-trail."}}}`)

	schemaOperatorIssueTokenInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["aid"],
"properties":{
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"}}}`)

	schemaOperatorIssueTokenOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["aid","jwt","expires_at"],
"properties":{
"aid":{"type":"string"},
"jwt":{"type":"string"},
"expires_at":{"type":"string","format":"date-time"}}}`)

	schemaRoleCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","permissions"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$","description":"Имя роли (kebab-case)."},
"description":{"type":"string","description":"Человекочитаемое описание роли."},
"permissions":{"type":"array","items":{"type":"string"},"description":"Permission-строки '<resource>.<action>' (+ optional ' on <selector>')."},
"default_scope":{"type":["string","null"],"description":"ADR-047 S1: scope-селектор (синтаксис per-perm-селектора, напр. 'coven=prod,stage'), наследуемый всеми permission-ами роли без своего селектора. null/omitted = роль без scope-ограничения (bare-perms unrestricted)."}}}`)

	schemaRoleDeleteInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`)

	schemaRoleUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","permissions"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"permissions":{"type":"array","items":{"type":"string"},"description":"Новый набор permissions (replace-семантика)."},
"default_scope":{"type":["string","null"],"description":"ADR-047 S1: заменить default_scope роли (null снимает scope). Ключ ОТСУТСТВУЕТ → scope не трогается (PATCH-семантика)."}}}`)

	schemaRoleGrantOperatorInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["role","aid"],
"properties":{
"role":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"}}}`)

	schemaRoleRevokeOperatorInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["role","aid"],
"properties":{
"role":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"}}}`)

	schemaRoleListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["roles"],
"properties":{
"roles":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","description","builtin","permissions","operators"],
"properties":{
"name":{"type":"string"},
"description":{"type":"string"},
"builtin":{"type":"boolean"},
"permissions":{"type":"array","items":{"type":"string"}},
"operators":{"type":"array","items":{"type":"string"}},
"default_scope":{"type":"string","description":"ADR-047 S1: default_scope роли (пусто = роль без scope)."}}}}}}`)

	schemaSynodCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$","description":"Имя Synod-группы (kebab-case)."},
"description":{"type":"string","description":"Человекочитаемое описание группы."}}}`)

	schemaSynodUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","description"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$","description":"Имя Synod-группы (kebab-case). PK immutable — адресует группу, не меняется."},
"description":{"type":"string","minLength":1,"maxLength":1024,"description":"Новое описание группы (заменяет старое; меняется ТОЛЬКО оно)."}}}`)

	schemaSynodDeleteInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`)

	schemaSynodAddOperatorInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["synod","aid"],
"properties":{
"synod":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"}}}`)

	schemaSynodRemoveOperatorInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["synod","aid"],
"properties":{
"synod":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"}}}`)

	schemaSynodGrantRoleInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["synod","role"],
"properties":{
"synod":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"role":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`)

	schemaSynodRevokeRoleInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["synod","role"],
"properties":{
"synod":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"role":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`)

	schemaSynodListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["synods"],
"properties":{
"synods":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","description","builtin","roles","operators"],
"properties":{
"name":{"type":"string"},
"description":{"type":"string"},
"builtin":{"type":"boolean"},
"roles":{"type":"array","items":{"type":"string"}},
"operators":{"type":"array","items":{"type":"string"}}}}}}}`)

	schemaIncarnationCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","service"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"service":{"type":"string"},
"covens":{"type":"array","items":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"},"description":"Declared env-Coven-метки incarnation (ADR-008 amendment a). Влияют на RBAC-scope create: оператор со scoped-permission incarnation.create on coven=X может создать только incarnation с covens в своём scope."},
"input":{"type":"object"}}}`)

	schemaIncarnationRunInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","scenario"],
"properties":{
"name":{"type":"string"},
"scenario":{"type":"string"},
"input":{"type":"object"}}}`)

	schemaIncarnationRunOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["_apply_id","incarnation","scenario"],
"properties":{"_apply_id":{"type":"string"},"incarnation":{"type":"string"},"scenario":{"type":"string"}}}`)

	schemaIncarnationGetInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string"}}}`)

	schemaIncarnationGetOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"description":"См. operator-api.md → IncarnationGetReply."}`)

	schemaIncarnationListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"service":{"type":"string"},
"status":{"type":"string","enum":["provisioning","ready","applying","error_locked","migration_failed","drift","destroying"]},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaIncarnationHistoryInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string"},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaIncarnationUnlockInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","reason"],
"properties":{
"name":{"type":"string"},
"reason":{"type":"string","minLength":1,"maxLength":500}}}`)

	schemaIncarnationRerunCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","reason"],
"properties":{
"name":{"type":"string"},
"reason":{"type":"string","minLength":1,"maxLength":500,"description":"Свободный текст подтверждения оператора; пишется в audit incarnation.create_rerun."}}}`)

	schemaIncarnationUnlockOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","previous_status","status","unlocked_by_aid","unlocked_at"],
"properties":{
"name":{"type":"string"},
"previous_status":{"type":"string"},
"status":{"type":"string"},
"unlocked_by_aid":{"type":"string"},
"unlocked_at":{"type":"string","format":"date-time"}}}`)

	schemaIncarnationUpgradeInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","to_version"],
"properties":{
"name":{"type":"string"},
"to_version":{"type":"string"}}}`)

	schemaIncarnationDestroyInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","allow_destroy"],
"properties":{
"name":{"type":"string"},
"allow_destroy":{"type":"boolean"}}}`)

	// check-drift: input override опционален (auto-from-state по конвенции
	// имени). Сами имена/типы override-параметров определяет converge-схема
	// сервиса, поэтому здесь — свободная map (additionalProperties не запрещаем).
	schemaIncarnationCheckDriftInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","description":"Имя incarnation."},
"input":{"type":"object","description":"Override converge-параметров; перекрывает auto-from-state по конвенции имени."}}}`)

	// DriftReport (ADR-031 Slice B): per-host агрегат task-результатов + summary.
	// Schema симметрична scenario.DriftReport (Go-тип в keeper/internal/scenario/checkdrift.go).
	schemaIncarnationCheckDriftOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["checked_at","incarnation","scenario_ref","hosts","summary"],
"properties":{
"checked_at":{"type":"string","format":"date-time"},
"incarnation":{"type":"string"},
"scenario_ref":{"type":"string","description":"Имя сценария Scry — 'converge'."},
"hosts":{"type":"array","items":{"type":"object","additionalProperties":false,
"required":["sid","status","tasks"],
"properties":{
"sid":{"type":"string"},
"status":{"type":"string","enum":["clean","drifted","unsupported","failed"]},
"tasks":{"type":"array","items":{"type":"object","additionalProperties":false,
"required":["idx","module","changed"],
"properties":{
"idx":{"type":"integer","minimum":0},
"module":{"type":"string"},
"action":{"type":"string"},
"changed":{"type":"boolean"},
"message":{"type":"string"}}}}}}},
"summary":{"type":"object","additionalProperties":false,
"required":["hosts_drifted","hosts_clean","hosts_unsupported","hosts_failed"],
"properties":{
"hosts_drifted":{"type":"integer","minimum":0},
"hosts_clean":{"type":"integer","minimum":0},
"hosts_unsupported":{"type":"integer","minimum":0},
"hosts_failed":{"type":"integer","minimum":0}}}}}`)

	schemaSoulCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","transport"],
"properties":{
"sid":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$","description":"SID (= FQDN хоста)."},
"transport":{"type":"string","enum":["agent","ssh"],"description":"agent — выпускается bootstrap-токен; ssh — без токена."},
"covens":{"type":"array","items":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"},"description":"Стабильные Coven-метки хоста."},
"note":{"type":"string"}}}`)

	schemaSoulCreateOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","transport","status","covens","registered_at","created_by_aid"],
"properties":{
"sid":{"type":"string"},
"transport":{"type":"string"},
"status":{"type":"string"},
"covens":{"type":"array","items":{"type":"string"}},
"registered_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":"string"},
"bootstrap_token":{"type":"string","description":"Только для transport=agent; выпускается один раз."},
"expires_at":{"type":"string","format":"date-time","description":"Только для transport=agent."}}}`)

	schemaSoulIssueTokenInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid"],
"properties":{
"sid":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},
"force":{"type":"boolean","description":"Истечь активный токен и выписать новый."}}}`)

	schemaSoulIssueTokenOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","bootstrap_token","expires_at"],
"properties":{
"sid":{"type":"string"},
"bootstrap_token":{"type":"string","description":"Выпускается один раз; клиент обязан сохранить."},
"expires_at":{"type":"string","format":"date-time"}}}`)

	// selector — подмножество словаря таргетинга soul.* (all/sids/coven/
	// incarnation/status), симметрично REST POST /v1/souls/coven. Свободный
	// CEL-предикат сознательно НЕ поддержан (ломает доказуемость scope-
	// проверки). Минимум один критерий обязателен (all=true ИЛИ один из
	// sids/coven/incarnation/status) — runtime-проверка в service-слое
	// (ErrBulkEmptySelector → validation-failed).
	//
	// `label` ↔ `labels` — XOR по mode: append/remove → label, replace →
	// labels[]. JSON Schema не выражает XOR-условие через if/then на mode
	// (валидатор клиента в произвольной реализации) — основная проверка
	// делается в handler/MCP (422 на нарушение).
	schemaSoulCovenAssignInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["mode","selector"],
"properties":{
"mode":{"type":"string","enum":["append","remove","replace"],"description":"append — добавить метку; remove — снять; replace — заменить набор Coven-меток целиком."},
"label":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$","description":"Назначаемая Coven-метка для append/remove (для append обязана быть в coven-scope оператора). Для replace используйте labels."},
"labels":{"type":"array","items":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"},"description":"Набор Coven-меток для mode=replace (может быть пустым = снять все). Каждая метка обязана быть в coven-scope оператора."},
"selector":{"type":"object","additionalProperties":false,"description":"Таргет хостов; пересекается с coven-scope оператора. Минимум один критерий обязателен.","properties":{
"all":{"type":"boolean","description":"Весь реестр (∩ scope). Без host-фильтра."},
"sids":{"type":"array","items":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},"description":"Точечный список SID."},
"coven":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$","description":"Хосты, у которых УЖЕ есть эта метка."},
"incarnation":{"type":"string","pattern":"^[a-z0-9][a-z0-9-]{0,62}$","description":"Хосты этой incarnation (имя incarnation как корневая Coven-метка, ADR-008)."},
"status":{"type":"string","enum":["pending","connected","disconnected","revoked","expired","destroyed"],"description":"Фильтр по статусу."}}},
"dry_run":{"type":"boolean","description":"true — вернуть matched под selector ∩ scope без UPDATE."}}}`)

	schemaSoulCovenAssignOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["mode","matched","changed","status","dry_run"],
"properties":{
"mode":{"type":"string"},
"label":{"type":"string","description":"Применённая метка для append/remove."},
"labels":{"type":"array","items":{"type":"string"},"description":"Применённый набор меток для replace."},
"matched":{"type":"integer","minimum":0,"description":"Хосты под selector ∩ scope."},
"changed":{"type":"integer","minimum":0,"description":"Фактически изменённые строки (0 для dry_run)."},
"status":{"type":"string","enum":["completed","partial"]},
"dry_run":{"type":"boolean"}}}`)

	schemaSoulListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"coven":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]},
"status":{"type":"string","enum":["pending","connected","disconnected","expired"]},
"transport":{"type":"string","enum":["agent","ssh"]},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaSoulSshTargetUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","ssh_port","ssh_user","soul_path"],
"properties":{
"sid":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},
"ssh_port":{"type":"integer","minimum":1,"maximum":65535},
"ssh_user":{"type":"string","minLength":1},
"soul_path":{"type":"string","pattern":"^/.+","description":"Абсолютный Unix-путь к soul-бинарю на хосте."}}}`)

	schemaSoulSshTargetUpdateOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","ssh_target"],
"properties":{
"sid":{"type":"string"},
"ssh_target":{"type":"object","additionalProperties":false,"required":["ssh_port","ssh_user","soul_path"],"properties":{
"ssh_port":{"type":"integer"},
"ssh_user":{"type":"string"},
"soul_path":{"type":"string"}}}}}`)

	schemaPluginAllowInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["namespace","name","ref"],
"properties":{
"namespace":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$","description":"Namespace плагина (kebab-case + точки/подчёркивание; без слешей)."},
"name":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$","description":"Имя плагина."},
"ref":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$","description":"Operator-asserted метка допуска (tag-ref вида v1.0.0). Branch-ref со слешем в MVP не поддержан."}}}`)

	schemaPluginAllowOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["namespace","name","ref","sha256"],
"properties":{
"namespace":{"type":"string"},
"name":{"type":"string"},
"ref":{"type":"string"},
"sha256":{"type":"string","description":"SHA-256 (hex) допущенного бинаря."}}}`)

	schemaPluginRevokeInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["namespace","name","ref"],
"properties":{
"namespace":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"},
"name":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"},
"ref":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"}}}`)

	schemaPluginListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sigils"],
"properties":{
"sigils":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["namespace","name","ref","sha256","allowed_by_aid","allowed_at","revoked_at"],
"properties":{
"namespace":{"type":"string"},
"name":{"type":"string"},
"ref":{"type":"string"},
"sha256":{"type":"string"},
"allowed_by_aid":{"type":"string"},
"allowed_at":{"type":"string","format":"date-time"},
"revoked_at":{"type":["string","null"],"format":"date-time"}}}}}}`)

	schemaSigilKeyIntroduceInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"make_primary":{"type":"boolean","description":"Сделать новый ключ primary (новые Sigil-ы подписываются им). По умолчанию false."}}}`)

	schemaSigilKeyIntroduceOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["key_id","pubkey_pem","is_primary","status","introduced_at"],
"properties":{
"key_id":{"type":"string","pattern":"^[0-9a-f]{64}$","description":"Стабильный id ключа: SHA-256(SPKI), hex."},
"pubkey_pem":{"type":"string","description":"Публичная часть (SPKI PEM). Приватник в ответе НЕ возвращается."},
"is_primary":{"type":"boolean"},
"status":{"type":"string"},
"introduced_at":{"type":"string","format":"date-time"}}}`)

	schemaSigilKeyIDInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["key_id"],
"properties":{
"key_id":{"type":"string","pattern":"^[0-9a-f]{64}$","description":"id ключа подписи (SHA-256(SPKI), hex)."}}}`)

	schemaSigilKeyListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["keys"],
"properties":{
"keys":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["key_id","is_primary","status","introduced_at"],
"properties":{
"key_id":{"type":"string"},
"is_primary":{"type":"boolean"},
"status":{"type":"string"},
"introduced_at":{"type":"string","format":"date-time"}}}}}}`)

	schemaServiceRegisterInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","git","ref"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$","description":"Имя Service-а (kebab-case)."},
"git":{"type":"string","description":"git-источник service-репо (URL; не секрет)."},
"ref":{"type":"string","description":"git ref (tag/branch) — версия Service-а (ADR-007)."},
"refresh":{"type":"string","description":"Опц. duration авто-refresh ('5m'); опущено — без авто-refresh."}}}`)

	schemaServiceUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","git","ref"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$","description":"Имя Service-а (ключ записи, не меняется)."},
"git":{"type":"string","description":"Новый git-источник (replace-семантика)."},
"ref":{"type":"string","description":"Новый git ref (replace-семантика)."},
"refresh":{"type":"string","description":"Опц. duration авто-refresh ('5m')."}}}`)

	schemaServiceDeregisterInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`)

	schemaServiceView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","git","ref","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"git":{"type":"string"},
"ref":{"type":"string"},
"refresh":{"type":"string"},
"created_by_aid":{"type":"string"},
"updated_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}`)

	schemaServiceListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["services"],
"properties":{
"services":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","git","ref","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"git":{"type":"string"},
"ref":{"type":"string"},
"refresh":{"type":"string"},
"created_by_aid":{"type":"string"},
"updated_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}}}}`)

	schemaOmenCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","source_type","endpoint","auth_ref"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Имя Omen-а (kebab-case)."},
"source_type":{"type":"string","enum":["vault","prometheus","elk"],"description":"Тип внешней системы."},
"endpoint":{"type":"string","description":"URL внешней системы (не секрет)."},
"auth_ref":{"type":"string","pattern":"^vault:","description":"vault-ref на master-credential (vault:<mount>/<path>); сам секрет не передаётся."}}}`)

	schemaOmenListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaOmenDeleteInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"}}}`)

	schemaOmenView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","source_type","endpoint","auth_ref","created_at"],
"properties":{
"name":{"type":"string"},
"source_type":{"type":"string","enum":["vault","prometheus","elk"]},
"endpoint":{"type":"string"},
"auth_ref":{"type":"string"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"}}}`)

	schemaOmenListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["omens","total"],
"properties":{
"total":{"type":"integer"},
"omens":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","source_type","endpoint","auth_ref","created_at"],
"properties":{
"name":{"type":"string"},
"source_type":{"type":"string","enum":["vault","prometheus","elk"]},
"endpoint":{"type":"string"},
"auth_ref":{"type":"string"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"}}}}}}`)

	// allow — свободный объект на уровне JSON Schema: его форма зависит от
	// source_type Omen-а (vault {paths?,policies?} / prometheus {queries} / elk
	// {indices}), и эту привязку без триггера декларативно не выразить —
	// runtime-валидация делает augur.ValidateAllow (augur.md §4.2).
	schemaRiteCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["omen","allow"],
"properties":{
"omen":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Omen, к которому относится grant."},
"coven":{"type":"string","description":"Субъект-grant по Coven-метке (XOR с sid)."},
"sid":{"type":"string","description":"Субъект-grant по конкретному SID (XOR с coven)."},
"allow":{"type":"object","description":"Allow-list; форма по source_type Omen-а: vault {paths?,policies?} / prometheus {queries} / elk {indices}."},
"delegate":{"type":"boolean","description":"false — брокер (MVP-1); true — делегация (MVP-2)."},
"token_ttl":{"type":"string","description":"TTL минтуемого scoped-токена; только vault-delegate."},
"token_num_uses":{"type":"integer","minimum":0,"description":"Лимит использований токена; только vault-delegate."}}}`)

	schemaRiteListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["omen"],
"properties":{
"omen":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"}}}`)

	schemaRiteDeleteInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["id"],
"properties":{
"id":{"type":"integer","minimum":1}}}`)

	schemaRiteView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["id","omen","allow","delegate","created_at"],
"properties":{
"id":{"type":"integer"},
"omen":{"type":"string"},
"coven":{"type":"string"},
"sid":{"type":"string"},
"allow":{"type":"object"},
"delegate":{"type":"boolean"},
"token_ttl":{"type":"string"},
"token_num_uses":{"type":"integer"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"}}}`)

	schemaRiteListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["rites"],
"properties":{
"rites":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["id","omen","allow","delegate","created_at"],
"properties":{
"id":{"type":"integer"},
"omen":{"type":"string"},
"coven":{"type":"string"},
"sid":{"type":"string"},
"allow":{"type":"object"},
"delegate":{"type":"boolean"},
"token_ttl":{"type":"string"},
"token_num_uses":{"type":"integer"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"}}}}}}`)

	// --- Oracle (Vigil / Decree, ADR-030 beacons) ---
	//
	// params (Vigil) / action_input (Decree) — свободные объекты на уровне JSON
	// Schema: их форма зависит от check / scenario, без триггера декларативно не
	// выразить (typed-payload отложен, ADR-030); runtime-валидация — service-слой.
	schemaOraclePaginatedInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaOracleNameInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"}}}`)

	schemaVigilCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","interval","check"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Имя Vigil-а (kebab-case)."},
"coven":{"type":"array","items":{"type":"string"},"description":"Субъект-метки coven (XOR с sid): Vigil раздаётся всем Soul-ам с любой из этих меток."},
"sid":{"type":"string","description":"Субъект — один конкретный SID (XOR с coven)."},
"interval":{"type":"string","description":"Частота проверки (duration-конвенция, напр. '30s')."},
"check":{"type":"string","description":"Адрес core-beacon (напр. 'core.beacon.file_changed')."},
"params":{"type":"object","description":"Параметры проверки; форма зависит от check (path / service-name / порог)."},
"enabled":{"type":"boolean","description":"Активна ли проверка. По умолчанию true."}}}`)

	schemaVigilView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","interval","check","params","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"coven":{"type":"array","items":{"type":"string"}},
"sid":{"type":"string"},
"interval":{"type":"string"},
"check":{"type":"string"},
"params":{"type":"object"},
"enabled":{"type":"boolean"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}`)

	schemaVigilListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["vigils","total"],
"properties":{
"total":{"type":"integer"},
"vigils":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","interval","check","params","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"coven":{"type":"array","items":{"type":"string"}},
"sid":{"type":"string"},
"interval":{"type":"string"},
"check":{"type":"string"},
"params":{"type":"object"},
"enabled":{"type":"boolean"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}}}}`)

	schemaDecreeCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","on_beacon","incarnation_name","action_scenario"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Имя Decree-а (kebab-case)."},
"on_beacon":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Имя Vigil-а, на чей Portent правило реагирует."},
"where":{"type":"string","description":"Опц. CEL-предикат над event.data (напр. 'event.data.severity == \"critical\"'); compile-проверяется на create."},
"coven":{"type":"array","items":{"type":"string"},"description":"Субъект-метки coven (XOR с sid): какие хосты могут триггерить правило."},
"sid":{"type":"string","description":"Субъект — один конкретный SID (XOR с coven)."},
"incarnation_name":{"type":"string","pattern":"^[a-z0-9][a-z0-9-]{0,62}$","description":"Таргет-incarnation реакции (ServiceRef резолвится из неё; обязательно)."},
"action_scenario":{"type":"string","pattern":"^[a-z][a-z0-9_]*$","description":"Named scenario (whitelist; raw-команда отвергнута)."},
"action_input":{"type":"object","description":"Вход сценария (vault-ref едет как есть)."},
"cooldown":{"type":"string","description":"Минимальный интервал между срабатываниями per-(decree, subject) (duration; опущено → выключен)."},
"enabled":{"type":"boolean","description":"Активно ли правило. По умолчанию true."}}}`)

	schemaDecreeView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","on_beacon","incarnation_name","action_scenario","action_input","cooldown","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"on_beacon":{"type":"string"},
"where":{"type":"string"},
"coven":{"type":"array","items":{"type":"string"}},
"sid":{"type":"string"},
"incarnation_name":{"type":"string"},
"action_scenario":{"type":"string"},
"action_input":{"type":"object"},
"cooldown":{"type":"string"},
"enabled":{"type":"boolean"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}`)

	schemaDecreeListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["decrees","total"],
"properties":{
"total":{"type":"integer"},
"decrees":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","on_beacon","incarnation_name","action_scenario","action_input","cooldown","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"on_beacon":{"type":"string"},
"where":{"type":"string"},
"coven":{"type":"array","items":{"type":"string"}},
"sid":{"type":"string"},
"incarnation_name":{"type":"string"},
"action_scenario":{"type":"string"},
"action_input":{"type":"object"},
"cooldown":{"type":"string"},
"enabled":{"type":"boolean"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}}}}`)

	schemaPushApplyInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["inventory","destiny"],
"properties":{
"inventory":{"type":"array","items":{"type":"string"},"minItems":1},
"destiny":{"type":"string","description":"<name>@<ref>"},
"input":{"type":"object"},
"ssh_provider":{"type":"string"},
"cleanup_stale_versions":{"type":"boolean"}}}`)

	schemaPushCleanupInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["inventory"],
"properties":{
"inventory":{"type":"array","items":{"type":"string"},"minItems":1},
"ssh_provider":{"type":"string"},
"full":{"type":"boolean"}}}`)

	schemaProviderCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","type","region","credentials_ref"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"type":{"type":"string"},
"region":{"type":"string"},
"credentials_ref":{"type":"string","pattern":"^vault:"}}}`)

	schemaProviderCreateOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","type","region","credentials_ref","created_at","created_by_aid"],
"properties":{
"name":{"type":"string"},
"type":{"type":"string"},
"region":{"type":"string"},
"credentials_ref":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":"string"}}}`)

	schemaProfileCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","provider","params"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"provider":{"type":"string"},
"params":{"type":"object"},
"cloud_init":{"type":"string"}}}`)

	// --- Errand (ADR-033) ---
	//
	// schemaErrandRow — base-форма строки errands (используется как
	// schemaErrandGetOutput напрямую и как элемент schemaErrandListOutput.items).
	schemaErrandRow = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["errand_id","sid","module","status","started_by_aid","started_at"],
"properties":{
"errand_id":{"type":"string","description":"ULID запуска."},
"sid":{"type":"string","description":"Целевой Soul (FQDN)."},
"module":{"type":"string","description":"Fully-qualified имя модуля."},
"status":{"type":"string","enum":["running","success","failed","timed_out","cancelled","module_not_allowed"]},
"exit_code":{"type":"integer","description":"Exit-код verb-модуля (NULL для read-safe non-shell)."},
"stdout":{"type":"string"},
"stderr":{"type":"string"},
"stdout_truncated":{"type":"boolean"},
"stderr_truncated":{"type":"boolean"},
"duration_ms":{"type":"integer"},
"error_message":{"type":"string"},
"output":{"type":"object","description":"Структурный output read-safe модулей; для shell/exec отсутствует."},
"started_by_aid":{"type":"string"},
"started_at":{"type":"string","format":"date-time"},
"finished_at":{"type":"string","format":"date-time","description":"Заполнен только для терминальных статусов."}}}`)

	schemaErrandRunInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","module"],
"properties":{
"sid":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$","description":"FQDN целевого Soul."},
"module":{"type":"string","description":"Адрес модуля core.<class>.<state> либо core.cmd.shell / core.exec.run."},
"input":{"type":"object","description":"Input модуля (форма зависит от модуля)."},
"timeout_seconds":{"type":"integer","minimum":1,"maximum":300,"description":"Server-cap полного timeout-а. Default 30."},
"dry_run":{"type":"boolean","description":"true → Soul зовёт mod.Plan вместо mod.Apply (только read-safe модули)."}}}`)

	schemaErrandRunOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["errand_id","sid","module","status","async"],
"properties":{
"errand_id":{"type":"string"},
"sid":{"type":"string"},
"module":{"type":"string"},
"status":{"type":"string","enum":["running","success","failed","timed_out","cancelled","module_not_allowed"]},
"async":{"type":"boolean","description":"true → server-cap превышен, дожимай через keeper.errand.get."},
"exit_code":{"type":"integer"},
"stdout":{"type":"string"},
"stderr":{"type":"string"},
"stdout_truncated":{"type":"boolean"},
"stderr_truncated":{"type":"boolean"},
"duration_ms":{"type":"integer"},
"error_message":{"type":"string"},
"output":{"type":"object"}}}`)

	schemaErrandListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"sid":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},
"status":{"type":"string","enum":["running","success","failed","timed_out","cancelled","module_not_allowed"]},
"started_after":{"type":"string","format":"date-time"},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaErrandListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["items","offset","limit","total"],
"properties":{
"items":{"type":"array","items":{"type":"object"}},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000},
"total":{"type":"integer","minimum":0}}}`)

	schemaErrandGetInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["errand_id"],
"properties":{
"errand_id":{"type":"string"}}}`)

	// schemaErrandCancelInput / Output — keeper.errand.cancel (ADR-033 slice E5).
	// Input — единственное поле errand_id (как у get); output — ack-объект,
	// зеркальный 204-эквивалент REST DELETE.
	schemaErrandCancelInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["errand_id"],
"properties":{
"errand_id":{"type":"string"}}}`)

	schemaErrandCancelOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["errand_id","cancelled"],
"properties":{
"errand_id":{"type":"string"},
"cancelled":{"type":"boolean"}}}`)

	// --- Voyage (ADR-043, S5) — input/output schemas. RBAC-by-kind в handler-е.
	schemaVoyageStartInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["kind","target"],
"properties":{
"kind":{"type":"string","enum":["scenario","command"]},
"scenario_name":{"type":"string","description":"Обязательно для kind=scenario."},
"module":{"type":"string","description":"Обязательно для kind=command (whitelist Soul-side)."},
"input":{"type":"object","description":"Параметры прогона (НЕ логируются в audit)."},
"target":{
"type":"object","additionalProperties":false,
"properties":{
"incarnations":{"type":"array","items":{"type":"string","pattern":"^[a-z0-9][a-z0-9-]{0,62}$"},"description":"scenario — имена инкарнаций."},
"service":{"type":"string","description":"scenario — фильтр incarnation.service."},
"sids":{"type":"array","items":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},"description":"command — SID-snapshot."},
"where":{"type":"string","maxLength":4096,"description":"command — CEL-предикат (MVP сохраняется без evaluate)."},
"coven":{"type":"array","items":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"},"description":"scenario — env-тег (any-of) / command — coven-метка (AND)."}
}
},
"batch":{"type":"string","examples":["20%"],"description":"Размер батча: N единиц | N% (1-100) от scope. Взаимоисключающе с batch_size — смешение → 422 voyage_batch_spec_conflict. grammar ^(\\d+)%?$."},
"max_failures":{"type":"string","examples":["25%"],"description":"Порог провалов: N абсолют | N% от единиц прогона. Взаимоисключающе с fail_threshold → 422 voyage_batch_spec_conflict."},
"batch_size":{"type":"integer","minimum":1,"deprecated":true,"description":"DEPRECATED — используйте batch. Размер Leg. null → весь прогон один Leg."},
"concurrency":{"type":"integer","minimum":1,"maximum":500,"description":"0/missing → default 50."},
"dry_run":{"type":"boolean"},
"schedule_at":{"type":"string","format":"date-time","description":"Отложенный старт → status scheduled."},
"inter_batch_interval_ms":{"type":"integer","minimum":0,"description":"Пауза между Leg-ами (мс)."},
"on_failure":{"type":"string","enum":["abort","continue"]}
}}`)

	schemaVoyageStartOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["voyage_id","kind","scope_size","status","location"],
"properties":{
"voyage_id":{"type":"string","description":"ULID."},
"kind":{"type":"string","enum":["scenario","command"]},
"scope_size":{"type":"integer","description":"Число резолвнутых единиц."},
"status":{"type":"string","enum":["pending","scheduled"]},
"location":{"type":"string","description":"REST path для get/poll (/v1/voyages/<id>)."}}}`)

	schemaVoyageGetInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["voyage_id"],
"properties":{
"voyage_id":{"type":"string"}}}`)

	schemaVoyageListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"kind":{"type":"string","enum":["scenario","command"]},
"status":{"type":"array","items":{"type":"string","enum":["scheduled","pending","running","succeeded","failed","partial_failed","cancelled"]}},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaVoyageView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":true,
"required":["voyage_id","kind","status","scope_size","total_batches","current_batch_index","started_by_aid","created_at"],
"properties":{
"voyage_id":{"type":"string"},
"kind":{"type":"string"},
"status":{"type":"string"},
"scope_size":{"type":"integer"},
"total_batches":{"type":"integer"},
"current_batch_index":{"type":"integer"},
"started_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"}}}`)

	schemaVoyageListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["items","offset","limit","total"],
"properties":{
"items":{"type":"array","items":{"type":"object","additionalProperties":true}},
"offset":{"type":"integer"},
"limit":{"type":"integer"},
"total":{"type":"integer"}}}`)

	schemaVoyageCancelOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["voyage_id","status"],
"properties":{
"voyage_id":{"type":"string"},
"status":{"type":"string","enum":["cancelled"]}}}`)

	schemaProfileCreateOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","provider","params","created_at","created_by_aid"],
"properties":{
"name":{"type":"string"},
"provider":{"type":"string"},
"params":{"type":"object"},
"cloud_init":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":"string"}}}`)

	// Push-Provider (S7-2) — input/output schemas. name-pattern symmetric с
	// pushprovider.NamePattern (`^[a-z][a-z0-9-]{0,62}$` — env-var-name-safe).
	schemaPushProviderCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]{0,62}$","description":"Имя плагина (= plugins.ssh_providers[].name)."},
"params":{"type":"object","description":"Opaque per-provider params. Sensitive-keys (secret_id/token/password/private_key) ОБЯЗАНЫ быть vault-refs (vault:<path>)."}}}`)

	schemaPushProviderUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","params"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]{0,62}$"},
"params":{"type":"object","description":"Полный новый набор params (replace-семантика)."}}}`)

	schemaPushProviderByNameInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]{0,62}$"}}}`)

	schemaPushProviderListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"name_pattern":{"type":"string","description":"LIKE-форма фильтра имени (например, vault%)."},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaPushProviderView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","params","created_at","updated_at","created_by_aid"],
"properties":{
"name":{"type":"string"},
"params":{"type":"object"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":"string"},
"updated_by_aid":{"type":["string","null"]}}}`)

	// Herald/Tiding (ADR-052, S4). 1:1 с REST-схемами openapi.yaml.
	schemaHeraldCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","type","config"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Имя Herald-канала (kebab-case)."},
"type":{"type":"string","enum":["webhook"],"description":"Тип канала (webhook в MVP)."},
"config":{"type":"object","description":"Per-type config (webhook — { url, опц. headers, опц. http_allowed/allow_private })."},
"secret_ref":{"type":["string","null"],"description":"Опц. vault-ref на signing-token (vault:<mount>/<path>); подпись webhook X-SoulStack-Signature."},
"enabled":{"type":"boolean","description":"Канал включён (опущено → true)."}}}`)

	schemaHeraldUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","type","config"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"},
"type":{"type":"string","enum":["webhook"]},
"config":{"type":"object","description":"Полный новый config (replace-семантика)."},
"secret_ref":{"type":["string","null"]},
"enabled":{"type":"boolean"}}}`)

	schemaHeraldByNameInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"}}}`)

	schemaHeraldListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaHeraldView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","type","config","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"type":{"type":"string","enum":["webhook"]},
"config":{"type":"object"},
"secret_ref":{"type":["string","null"]},
"enabled":{"type":"boolean"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":["string","null"]}}}`)

	schemaTidingCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","herald","event_types"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"},
"herald":{"type":"string","description":"Имя Herald-канала доставки (FK)."},
"event_types":{"type":"array","items":{"type":"string"},"description":"area-glob scenario_run.* в scope прогонов (scenario_run/command_run/voyage/cadence + incarnation.drift_checked)."},
"only_failures":{"type":"boolean"},
"only_changes":{"type":"boolean"},
"incarnation":{"type":["string","null"]},
"cadence":{"type":["string","null"]},
"task":{"type":["string","null"],"description":"Опц. селектор подписки на конкретную задачу по адресу register∪id из changed_tasks (ADR-052 §l)."},
"enabled":{"type":"boolean","description":"Опущено → true."}}}`)

	schemaTidingUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","herald","event_types"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"},
"herald":{"type":"string"},
"event_types":{"type":"array","items":{"type":"string"}},
"only_failures":{"type":"boolean"},
"only_changes":{"type":"boolean"},
"incarnation":{"type":["string","null"]},
"cadence":{"type":["string","null"]},
"task":{"type":["string","null"],"description":"Опц. селектор подписки на конкретную задачу (register∪id из changed_tasks, ADR-052 §l). Replace — отсутствие очищает."},
"enabled":{"type":"boolean"}}}`)

	schemaTidingByNameInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"}}}`)

	schemaTidingListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"include_ephemeral":{"type":"boolean","description":"true → включить разовые (ephemeral) правила; по умолчанию false (скрыты)"},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaTidingView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","herald","event_types","only_failures","only_changes","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"herald":{"type":"string"},
"event_types":{"type":"array","items":{"type":"string"}},
"only_failures":{"type":"boolean"},
"only_changes":{"type":"boolean"},
"incarnation":{"type":["string","null"]},
"cadence":{"type":["string","null"]},
"task":{"type":["string","null"]},
"enabled":{"type":"boolean"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":["string","null"]}}}`)
)
