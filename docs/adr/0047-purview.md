# ADR-047. Purview — scoped RBAC-видимость узлов (role default_scope + расширенный селектор)

**Контекст.** До этого ADR RBAC-scope существовал ровно в одном измерении и одном месте: `Enforcer.CovenScope` ([`keeper/internal/rbac/enforcer.go`](../../keeper/internal/rbac/enforcer.go)) возвращал `(covens []string, unrestricted bool)` — союз `coven=`-значений по матчащим permission-ам плюс bool «без ограничений». Этого достаточно для bulk-coven-mutation, но не покрывает реальную потребность: оператор `db-operator` при `GET /v1/souls` всё равно видит **весь** флот, а не только свой scope; `incarnation.list`, target Cadence/Vigil/Voyage тоже игнорируют scope. `.*`-таргет означал «весь флот», а не «всё, что доступно мне». Расширять scope нужно не только по coven, но и по regex (SID), soulprint-предикату (CEL `soulprint.self.*`) и state-предикату (CEL по `incarnation.state`) — а `CovenScope` с его `(covens, bool)`-сигнатурой это не вмещает.

**Решение.** Вводится сущность **Purview** — резолвер разрешённого scope оператора. Контракт:

```
ResolveScope(aid, resource, action string) → Purview
```

`Purview` — типизированный результат с измерениями: `covens` (точные метки), `regexes` (по SID, RE2), `soulprint` (CEL-предикаты `soulprint.self.*`), `state` (CEL-предикаты по `incarnation.state`), плюс терминальные флаги `unrestricted` (нет ограничений вообще) и `deny` (доступ запрещён). Purview **обобщает** `Enforcer.CovenScope`: covens+unrestricted остаются частным случаем (`coven`-измерение + флаг). Несколько значений в измерении (multi-coven, multi-regex) — союз (OR внутри измерения).

**(а) default_scope на уровне роли.** Новое поле `rbac_roles.default_scope` — scope-селектор, наследуемый **всеми** permission-ами роли. Per-permission селектор (`on coven=X` в строке permission) **переопределяет** default_scope для конкретной permission. То есть: роль задаёт базовый scope один раз, отдельные permissions могут его сузить/сменить точечно.

**(б) Default-deny по измерениям с тремя исключениями.** Если роль **явно вводит** scope-измерение (через `default_scope` или per-perm-селектор), но конкретное измерение в нём пусто → на этом измерении **deny**. НО:
- **`*`-permission (cluster-admin)** = явный allow-all, НЕ подчиняется default-deny (`*` буквально значит «всё», иначе bootstrap cluster-admin залочился бы — [ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)/[ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).
- **bare-permission** (без `default_scope` роли И без per-perm-селектора) = `unrestricted` (обратная совместимость: существующие роли не ломаются — это та же семантика, что у `Permission.Matches` при `Selector==nil` → `true` и у `CovenScope` при bare-permission → `unrestricted`).
- Пустое измерение даёт deny только когда оно **введено явно** — отсутствие измерения ≠ пустое измерение.

Это снимает blocker «не залочить bootstrap cluster-admin при включении default-deny».

**(в) Расширение селектора (будущие слайсы).** К exact-ключам `coven`/`host`/`incarnation`/`service` ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)) добавляются: `regex` (по SID, RE2 без backtracking), `soulprint` (CEL-предикат `soulprint.self.*`, [ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)/[ADR-018](0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)), `state` (CEL-предикат по `incarnation.state`). Конкретная грамматика ключей в permission-строке — **propose-and-wait при реализации соответствующего слайса** (S2).

**(г) Scoped-видимость везде.** souls-list, incarnations-list, target Cadence/Vigil/Voyage отдают/таргетируют **только Purview** оператора. `.*` = «все доступные МНЕ», а не весь флот. Связь с invocation-time scope: regex+where в target ([ADR-040](0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)/[ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)) пересекается с Purview — Purview есть **верхняя граница**, а invocation только сужает. Security-инвариант «invocation сужает» ложится естественно.

**Двухслойная авторизация read-эндпоинтов (ADR-047 §г amendment 2026-06-04).** Read-видимость узлов авторизуется в ДВА слоя, обслуживаемых разными методами enforcer-а:
1. **Gate (existence)** — `HoldsAction(aid, resource, action)`: держит ли оператор действие хоть в каком-то scope (`ResolvePurview` непуст: `Unrestricted` ИЛИ заполнено любое измерение coven/regex/soulprint/state, И `!Deny`). Не держит → 403 до handler-а. Gate scope-context-ом НЕ оперирует — read-эндпоинт не несёт host/coven/state в request-е на этапе middleware (scope резолвится из строк БД, которых на gate-этапе ещё нет).
2. **Scope-сужение (handler)** — `ResolvePurview` + per-resource резолверы (`soulpurview`/`statepredicate`): после фетча строк handler оставляет только узлы в scope-границе оператора (coven-pushdown / regex-keyset / state-CEL). Узел вне scope → исключён из списка / 404 на single-read (не палим существование).

**Инвариант:** scope-aware `Check(...,ctx)` для read-видимости НЕ применяется на gate-слое — он отвечает «применима ли permission в данном scope-контексте», а gate read-эндпоинта спрашивает «держит ли оператор действие в принципе». `Check(...,nil)` для scoped-permission даёт ложный deny (selector-ключ отсутствует в nil-контексте), что ломало доступ scoped-оператора к собственному списку (закрывается слайсами G0–G2). Mutating-эндпоинты с известным из тела/path scope-контекстом (`incarnation.run on coven=…`, `soul.coven-assign`) продолжают использовать scope-aware `Check`/`RequirePermissionMulti` — у них gate И scope совпадают в одном вопросе. Per-host history-эндпоинты (`GET /v1/souls/{sid}/history`) проходят тот же handler-InScope-gate, что get/soulprint.

**Slice-карта.**
- **S0** (этот ADR + Purview-рефактор): ввести `ResolveScope`/`Purview` как обобщение `CovenScope`, **БЕЗ смены семантики `Permission.Matches`** — pure refactor, наблюдаемое поведение не меняется.
- **S1**: `default_scope` роли + наследование/override + default-deny с `*`-исключением (амендит семантику «пустой селектор = allow» из [ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)).
- **S2**: расширение селектора по одному измерению: `regex` → `soulprint` → `state`.
- **S3**: scoped-видимость list→target.
- **S4**: объединить с regex+where late-eval target ([ADR-040](0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)/[ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)).

**Грабли (решить ДО соответствующего слайса).**
- **Перф CEL-scope-фильтра на souls-list** — до 100k evals на запрос. Нужна перф-стратегия (SQL-pushdown по `coven`/`regex` vs page-limited CEL по `soulprint`/`state`) — **решить в ADR/дизайне ДО S2b/S3**.
- **`subset.go` least-privilege** (выдача прав ⊆ собственных, [`keeper/internal/rbac/subset.go`](../../keeper/internal/rbac/subset.go)) должен расширяться на **каждое** новое scope-измерение, иначе оператор выдаст право шире своего → эскалация.
- **Regex ReDoS** — RE2 без backtracking безопасен по своей природе, но длину паттерна ограничить.
- **Default-deny миграция cluster-admin** — встроенный `cluster-admin` (`*`) не должен залочиться при включении default-deny (исключение (б)).

**Связь с ADR.** [ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres) (RBAC storage; S1 меняет семантику `Matches` «пустой селектор = allow»), [ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)/[ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) (identity/bootstrap — не залочить cluster-admin), [ADR-040](0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)/[ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) (target-scope), [ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)/[ADR-018](0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp) (CEL/soulprint), [ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) (coven).

**Amendment (2026-06-09, Synod — группа архонов [ADR-049](0049-synod.md#adr-049-synod--группа-архонов)).** С вводом промежуточного уровня **[Synod](0049-synod.md#adr-049-synod--группа-архонов)** (группа архонов, бандлящая роли) набор ролей оператора = прямые ∪ через Synod. Для Purview это **прозрачно**: объединение собирается в snapshot-сборке enforcer-а ([ADR-028(d)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)) **до** резолва — `ResolveScope`/`ResolvePurview`/`HoldsAction` получают уже-объединённый набор ролей, **матчинг-слой Purview не меняется** (Synod невидим ниже снимка). **Group-scope НЕ вводится:** у Synod нет собственного `default_scope` — **scope живёт на ролях** ([ADR-047(а)](#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор) `rbac_roles.default_scope` + per-perm-селектор), Synod лишь группирует уже-scoped роли и к scope-резолву ничего не добавляет. Второй scope-слой на группе (пересечение group-scope ∩ role-scope) — отвергнут в MVP как additive-расширение без подтверждённой потребности ([ADR-049](0049-synod.md#adr-049-synod--группа-архонов) §Отвергнутые (а)). **Least-privilege subset** ([§Грабли](#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор), [`subset.go`](../../keeper/internal/rbac/subset.go)) обязан считать эффективные права инициатора как прямые ∪ через Synod — иначе оператор выдаст право шире/уже своего ([ADR-049 §f](0049-synod.md#adr-049-synod--группа-архонов)).

#### S3b — scoped-видимость list-эндпоинтов (souls + incarnations)

Конкретизация слайса S3 для **read-видимости** (отдельно от S4 target). S3 разбит на S3a (target, объединение с late-eval) и **S3b (видимость list/get)**, потому что эти два пути имеют разный security-инвариант и разную перф-стратегию.

**Сущности.**
- **`keeper/internal/soulpurview`** (новый, ADR-047 S3b) — souls-аналог `keeper/internal/statepredicate`. Резолвер: принимает уже-резолвнутый `rbac.Purview` **параметром** (НЕ ходит в enforcer — `ResolvePurview` зовёт handler; S4 target-фильтр переиспользует тот же перевод поверх своего Purview-пересечения) → переводит scope-границу в параметры souls-запроса. Однонаправленная зависимость `soulpurview→rbac`, без import-cycle.
- **incarnations-list/get** scoped-видимость **переиспользует** существующий `statepredicate.ResolveIncarnations` (тот уже умеет SQL-pushdown по `service`/`coven` + page-by-page CEL по state) — нового пакета incarnations не вводит.

**Эндпоинты в скоупе S3b.** `GET /v1/souls` (list) + `GET /v1/souls/{sid}` (get) + `GET /v1/incarnations` (list) + `GET /v1/incarnations/{name}` (get). Каждый отдаёт **пересечение** запрошенного с Purview оператора для `<resource>.list` (souls-get/soulprint/history ходят под тем же `soul.list`-permission — паттерн service/omen/vigil).

**Покрытие резолвером (последовательность реализации).**
1. **S3b-0 (pilot, этот срез) — souls-list, только coven-измерение.** SQL-pushdown `souls.coven && ARRAY[purview.Covens]` (переиспользует `appendScopeClause`, общий с bulk coven-assign — единая fail-closed-семантика souls-слоя). offset/total корректны без дрейфа (coven-pushdown полон), keyset не нужен. Wiring: `SoulHandler.List` резолвит `ResolvePurview(aid, "soul", "list")` → `soulpurview.Resolve` → `soul.ListScope` → `soul.SelectAll`. Прежний `CovenScoper` (S0 `(covens, unrestricted)`) **обобщён** на `PurviewResolver` (`ResolvePurview`) — один резолвер для list-видимости И bulk coven-assign (тот разворачивает coven-измерение Purview в `BulkScope`).
2. **S3b-1** — souls single-get (`GET /v1/souls/{sid}`): тот же Purview, single-host membership-check.
3. **S3b-2** — regex/soulprint измерения для souls: **page-by-page CEL-постфильтр** поверх coven-сужённого набора + **keyset-курсор** (offset-пагинация по Go-постфильтру дрейфует — нужен keyset). Закрывает грабли «перф CEL-scope-фильтра на souls-list» (ADR-047 §Грабли): coven → SQL-pushdown сейчас, soulprint/regex → page-CEL+keyset.
4. **S3b-3** — incarnations list+get: переиспользуют `statepredicate.ResolveIncarnations`; incarnation-scope = `coven ∪ {incarnation.name}` (имя инкарнации — корневая Coven-метка, [ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)).

**fail-closed (security-инвариант, ПРОТИВОПОЛОЖНО presence fail-safe).** При неопределённости scope результат — ПУСТОЙ список, НЕ весь флот:
- пустой Purview (`Purview{}`: ни одного измерения, не `Unrestricted`) → пусто;
- нет claims / scoper не сконфигурирован → пусто;
- **scope-eval-error fail-CLOSED** — ошибка вычисления scope (битый CEL на runtime, сбой резолвера) скрывает, НЕ показывает.

Это **явно противоположно** presence-overlay `GET /v1/souls` (`overlayPresence`): при сбое Redis presence fail-SAFE (отдаёт PG-снимок). scope при сомнении **скрывает**, presence при сомнении **показывает** — два слоя НЕ заимствуют стратегию друг у друга. Порядок: scope сужает набор ДО presence-overlay-я (scope-фильтр в SQL, presence — поверх уже-scoped набора).

`Unrestricted`/`*`-permission → весь доступный список без scope-фильтра (bare-без-default → `Unrestricted`, backcompat).

**Граница S3b ↔ S4.** S3b — **видимость** (что оператор ВИДИТ в list/get). S4 — **target** (что оператор может ТРОНУТЬ в прогоне): invocation-time scope (regex+where, [ADR-040](0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)/[ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)) пересекается с Purview как с верхней границей. Оба читают один `ResolvePurview`; различие — в применении (read-фильтр vs target ∩ Purview).

**Перф.** coven — SQL-pushdown (сейчас, S3b-0). regex/soulprint — page-CEL-постфильтр + keyset (S3b-2). incarnations — `statepredicate.ResolveIncarnations` page-by-page (S3b-3). 100k-флот: coven-сужение в SQL отрезает основную массу до CEL-eval-а.

#### S4 — target ∩ Purview для command-пути Voyage (security-fix)

Конкретизация слайса S4 ([§Slice-карта](#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)) для **command-пути** Voyage (`kind=command`, permission `errand.run`, [ADR-043 §6](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)).

**Проблема (security).** Command-путь резолвит таргет как **bare NoSelector** и раскрывает его **cluster-wide** — без пересечения с Purview. Scoped-Архонт с правом `errand.run on coven=A` мог запустить command-Voyage на `coven=B` (его таргет не урезался до его scope). Это **асимметрия со scenario-путём** (`kind=scenario`, `incarnation.run`), где per-incarnation scope-check уже стоит и таргет не выходит за границу оператора. Command-путь — дыра эскалации.

**Решение.** Command-резолв таргета **пересекается с Purview** оператора через **переиспользование `soulpurview`** ([§S3b](#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)) — того же резолвера, что фильтрует `GET /v1/souls`. Покрытие командой = разрешённый запросом таргет **∩** `ResolvePurview(aid, "soul", …)` (верхняя граница, как в [§г](#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор): «invocation сужает»). Пересечение — **в резолвере**, не в handler-е: `POST /v1/voyages/preview` ([ADR-043 amendment](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)) **наследует** урезанный scope автоматически (общий резолвер).

**Новых RBAC-селектор-ключей НЕ вводится.** Пересечение реюзит существующие измерения Purview (coven / host / regex / soulprint / state); ключи RBAC-селектора остаются ограничены {`service`, `coven`, `incarnation`, `host`} ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)/[§в](#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)) — командный путь не добавляет своих.

**Семантика — гибрид (выбор пользователя 2026-06-09).** Три ветки по форме таргета:
1. **Явный чужой хост в `sids[]`** (оператор перечислил конкретные SID, часть вне его Purview) → **403** (anti-escalation, parity со scenario-путём, где явный выход за scope = отказ, не молчаливое усечение). Явное указание чужого хоста — попытка эскалации, а не широкий фильтр.
2. **Широкий target** (`coven=…` / `where:`-предикат, late-binding) → **урезать** до `target ∩ Purview` (как list-видимость: оператор получает то, что внутри его границы, без отказа).
3. **Пустое пересечение** (после урезания не осталось хостов) → **422 `voyage_empty_target`** (валидный запрос, но нечего исполнять — отличаем от 403-эскалации).

**Existence-gate — единый, через `ResolvePurview("errand","run")` (паттерн [§г G1](#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)).** «Держит ли оператор право `errand.run` хоть в каком-то scope» проверяется **тем же резолвом**, что даёт scope-границу для пересечения с таргетом — без отдельного предварительного bare-check-а: `Scope.Empty` (пустой Purview: ни одного измерения и не `Unrestricted`) = права нет ни в одном контексте → отказ ДО резолва таргета. Это устраняет грабли nil-context bare-check для scoped-роли (как souls G1: одиночная `Check(nil)` ложно денит роль с непустым scope). **Причина отказа классифицируется** (`Scope.Empty` сам её не несёт — ревокация и no-perm слиты в резолвере): через enforcer внутри ветки `Empty` — **revoked → `TypeOperatorRevokedToken` (401)**, **no-perm → 403** (`operator lacks required permission errand.run`). Классификатор безопасен (nil-context): scope уже доказан пустым в любом контексте, ложного деная scoped-роли быть не может — enforcer тут только различает причину, не второй gate. Парити error-семантики со scenario-путём (`incarnation.run`) и cluster-wide-веткой (scoper не сконфигурирован).

**soulprint/state — `Partial` до S3b-2b.** В command-∩-Purview измерения `soulprint`/`state` остаются **под-показом** (`Partial` — никогда over-show: при неполной поддержке измерения резолвер скорее урежет лишнее, чем покажет чужое) до закрытия S3b-2b; coven/regex/host работают полноценно. Это безопасное направление ошибки (fail-closed, [§S3b fail-closed](#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)).

**Совместимость.** Для **scoped-ролей** — это **смена поведения** (ранее cluster-wide command, теперь ∩ Purview): фиксируется как **security-fix в release-notes**. Для **Unrestricted / cluster-admin** (`*`-permission) — **без изменений** (`Unrestricted` → весь флот, как раньше — [§б](#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)).

**Граница S4 ↔ S3b.** S3b — **видимость** (что оператор ВИДИТ в list/get). S4 — **target** (что оператор может ТРОНУТЬ): и scenario-, и command-путь Voyage пересекают invocation-target с Purview как верхней границей. Оба читают один `ResolvePurview`; S4-command закрывает асимметрию, добивая command-путь до того же инварианта, что scenario.

**Slice-план P.** S-P0 — канон (этот амендмент). S-P1 — scoper-wire + `ResolveSIDsInScope` (резолв ∩ Purview для command-таргета). S-P2 — `createCommand` (три ветки гибрид-семантики) + guard-тесты (403-явный-чужой / урезание широкого / 422-пустое / Unrestricted-без-изменений). S-P3 — e2e (scoped-Архонт не выходит за coven, parity со scenario).
