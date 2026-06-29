# ADR-061. Единый-прогон provision→онбординг→роль: onboarding-await + mid-run re-resolve roster

> **Статус: active.** Решение пользователя + дизайн architect-а (эпик «Путь-2», слайсы S0 (этот ADR) / S1 (пилот await_online) / S2 (Stratify-passage-граница) / S3 (фактический re-resolve roster в run.go)). Канон фиксируется docs-first ДО кода; этот ADR **amends [ADR-009](0009-scenario-dsl.md), [ADR-056](0056-staged-render-passage.md), [ADR-006](0006-cache-redis.md), [ADR-017](0017-keeper-side-core.md)**. propose-and-wait закрыт (новые способности — флаги на существующем `core.soul.registered`, не новая сущность).
>
> **Прогресс имплементации.** S1 (пилот `await_online`) реализован: блокирующий poll Redis SID-lease + B1-strict fail + list-SID + потолок `keeper.yml::max_await_timeout`. S2 (Stratify-passage-граница для `refresh_soulprint`) и S3 (фактический re-resolve roster в run.go) **реализованы**: `refresh_soulprint: true` делает шаг passage-определяющим эмиттером, scenario-runner пере-резолвит roster на refresh-границе (live-снимок, см. §S3), `register.<name>.refreshed` эхает значение флага. WB cloud e2e — отдельный слайс.

## Amendment 2026-06-28 — no_hosts пропускается для provision-from-zero (два класса)

**Проблема.** Безусловный no_hosts-гейт (`run.go` шаг 3: `if len(hosts)==0 { abort no_hosts }`) срубал cloud-bootstrap create на пустом roster ДО keeper-dispatch. Целевой провижн-сценарий (`example-cloud-bootstrap/scenario/create`, см. [ADR-017(h)](0017-keeper-side-core.md), [ADR-063](0063-bootstrap-token-delivery.md)) на старте хостов НЕ имеет — он их и создаёт (`core.cloud.created` → `core.bootstrap.delivered`, обе `on: keeper`). Chicken-egg: run требует connected-хостов, run же их и провиженит. Гейт делал provision-from-zero неисполнимым.

**Решение (Вариант A + 2-й класс bypass).** Гейт пропускается при ОДНОМ ИЗ двух признаков provision-from-zero; иначе — `no_hosts` БИТ-В-БИТ:

```
provisionsRoster := config.HasRefreshEmitter(scn.Tasks)
if len(hosts)==0 && !allKeeperTasks(scn.Tasks) && !provisionsRoster { abort no_hosts }
```

- **Класс (а) — all-keeper** (`allKeeperTasks` по `scn.Tasks` ПОСЛЕ `ExpandIncludes`, каждая `render.IsKeeperTask`). Признак — все-`on:keeper`-состав сценария (VM создаются keeper-задачами, хостов на старте нет по определению). По СОСТАВУ, не по флагу — лишней DSL-поверхности не вводится. Пустой сценарий (`len(tasks)==0`) → `allKeeperTasks` = `false`: «нет задач» не повод bypass-ить гейт.
- **Класс (б) — mixed с refresh-эмиттером** (`config.HasRefreshEmitter(scn.Tasks)`: план несёт `core.soul.registered` с `refresh_soulprint: true`, рекурсивно через `block:`). Roster пере-резолвится **mid-run** (§S2/§S3): host-задачи деплоя стратифицируются в Passage **ПОСЛЕ** refresh-границы и видят уже пере-резолвленный live-снимок (онбордившиеся VM), а не пустой P0. Поэтому пустой стартовый roster **законен** — это и есть staged provision→роль одним прогоном. Детектор переиспользует существующий приватный предикат `taskIsRefreshEmitter` (тот же, что питает roster-ось `Stratify`/`RefreshBoundaries`) — module-name+param-parse НЕ дублируется.
- **Mixed БЕЗ refresh-эмиттера** (host-задача + keeper-задача, но `refresh_soulprint` нет) — `allKeeperTasks`=false И `HasRefreshEmitter`=false → **держит no_hosts**. Корректно: host-задача на пустом P0 без mid-run-перерезолва есть no_hosts. Объединять provision и роль в один НЕ-staged Passage (без refresh-эмиттера) по-прежнему нельзя.

**Essence keeper-контекст при пустом roster.** Essence-резолв брал OS-family/Covens представителя `hosts[0]` — на пустом roster паникнул бы. При пустом roster essence резолвится в **keeper-контексте** (`keeperEssenceInput`): default-слой + Coven-overlay инкарнации (корневая Coven-метка = `inc.Name`, [ADR-008](0008-coven-stable-tags.md): каждый хост roster-а несёт её, поэтому применима и без хоста) + `spec.essence`-override. OS-family overlay **пропускается** (нет per-host soulprint) — симметрично `renderKeeperTask`, рендерящему keeper-задачи без per-host soulprint. После онбординга созданных VM последующие Passage получают per-host essence обычным путём (§S3).

**Открыто.** standalone-recovery долгого `await_online`-барьера single-keeper-прогона — **не закрыт** (см. §«HA — provision-сценарии через Voyage» ниже): provision-сценарии рекомендуется гнать через Voyage.

**Cross-ref:** [ADR-017(h)](0017-keeper-side-core.md) (cloud-init B-flat + `core.cloud.created` generate_userdata), [ADR-063](0063-bootstrap-token-delivery.md) (`core.bootstrap.delivered`).

## Amendment 2026-06-29 — size-asserts деплой-веток гейтятся `when: !provision.enabled`

**Проблема.** Redis-сервис несёт keeper-side render-time **size-asserts** (`assert: size(soulprint.hosts) == shards*(1+replicas)` для cluster, `== 1+replicas` для sentinel — топология-guard, [ADR-009 amendment 2026-06-23](0009-scenario-dsl.md)). Pre-flight `EvalAsserts` (не-staged, [pre-flight-инвариант](0056-staged-render-passage.md)) вычисляет их **до** старта прогона. На provision-пути (`input.provision.enabled`) souls на этот момент **ещё не созданы** — их поднимут шаги `redis-provision.yml` (`core.cloud.created` → VM → `await_online`-барьер → `refresh_soulprint`), поэтому `size(soulprint.hosts)` на pre-flight равен 0 → assert падал `ErrAssertFailed` (422) ДО того, как кластер вообще существует. Тот же chicken-egg, что закрыл no_hosts-bypass (amendment 2026-06-28), но на другом гейте — pre-flight assert, а не run.go no_hosts.

**Решение (Вариант 1 — provision-aware гейт в redis-сервисе, локально).** На size-assert-задачи деплой-веток добавлен `when: "!(has(input.provision) && input.provision.enabled)"` (инверсия include-when provision-тела). Предикат **STATIC** ([`isStaticWhen`](../../keeper/internal/render/pipeline.go): чистый input, без `register.*`/`soulprint.self`) → pre-flight вычисляет его сам и при provision placeholder-skip-ает assert (НЕ `ErrAssertFailed`). На существующем roster-е (`provision` опущен/`enabled=false`) `has(input.provision)` даёт false → `when=true` → size-guard **активен как раньше** (НЕ ослаблен для НЕ-provision прогонов).

- **Где именно.** `create/cluster.yml`, `create/sentinel.yml`, `migrate_cluster/cluster.yml` (все три инклудят общее provision-тело `redis-provision.yml`). `migrate_cluster/sentinel.yml` собственного size-guard НЕ несёт (сразу `include redis-deploy-sentinel.yml`) — гейтить нечего. `create_from_souls/*` provision-тело НЕ инклудят (always-existing-roster) → их size-guard БЕЗ гейта (корректно: provision там невозможен).
- **Почему гейт безопасен.** При provision roster-инвариант гарантируется **не pre-flight-ом**, а двумя другими механизмами: (1) `count`-формула `redis-provision.yml` (число создаваемых VM = той же `shards*(1+replicas)` / `1+replicas` — единственный источник истины размера, отдельного `node_count` нет намеренно); (2) блокирующий `await_online`-барьер (B1-strict: недобор online → `failed` → `error_locked`). Симметрично no_hosts-bypass: provision-путь обслуживается составом сценария + барьером, а не up-front-проверкой пустого roster.
- **`pre-flight` остаётся не-staged (инвариант сохранён).** Фикс — на УРОВНЕ сервиса (DSL `when:`-гейт), keeper `EvalAsserts`/`PreflightAssert` НЕ трогается. Static-when placeholder-skip — уже существующий механизм pre-flight (`evalAssertTask` → `isStaticWhen`), а не staged-pre-flight.
- **Known-edge (degraded UX, НЕ silent-corruption).** При `cluster_topology` + provision `count` provision выводится из `shards`/`replicas` (формула `redis-provision.yml`), а НЕ из суммы размеров топологии (что использует size-guard на существующем roster-е). При расхождении этих двух (оператор задал `cluster_topology`, сумма размеров которой ≠ `shards*(1+replicas)`) provision поднимет VM по формуле, а раскладку плагин сверит по топологии → расхождение ловит **`await_online`-барьер + плагин fail-fast** (`community.redis.cluster` сверяет число нод), НЕ pre-flight. Это поздний внятный отказ (degraded UX), не тихая порча — приемлемо для edge `cluster_topology`+provision.

**Cross-ref:** amendment 2026-06-28 (no_hosts-bypass — родственный chicken-egg на run.go).

**Контекст.** Целевой сценарий — **один create-scenario** разворачивает N-шардовый кластер из «ничего»:

1. `core.cloud.provisioned` (`on: keeper`, [ADR-017](0017-keeper-side-core.md)) создаёт N VM, register-output несёт их `sid` / bootstrap-токены / cloud-метаданные;
2. созданные VM онбордятся (cloud-init + CSR-bootstrap, [ADR-012](0012-keeper-soul-grpc.md)), их `soul`-агенты поднимают EventStream к Keeper-у;
3. последующие задачи сценария применяют redis-роль к **уже онбордившимся** хостам — `on: [incarnation.name]`, `where: …`, чтение `soulprint.hosts`.

Этот сценарий **не работает** на текущем движке по трём связанным причинам:

- **`soulprint.hosts` — снимок на старте прогона.** Roster инкарнации резолвится один раз перед первым Passage и в пределах прогона не растёт. VM, созданные шагом (1), в roster шага (3) **не попадают** — `soulprint.hosts` их не видит, `on: [incarnation.name]` их не таргетит.
- **`refresh_soulprint` — заглушка.** Флаг на `core.soul.registered` принимается, но игнорируется (`register.<name>.refreshed` всегда `false`, [ADR-017](0017-keeper-side-core.md), `keeper/internal/coremod/soul/registered.go`). Нет способа сказать движку «перечитай roster».
- **Нет барьера онбординга.** Между «VM создана» и «`soul`-агент на ней online» проходит время (boot + cloud-init + bootstrap). Сценарий не может дождаться, пока созданные хосты станут online, прежде чем применять к ним роль — нет блокирующего шага-барьера.

**Решение.** Две способности на существующем keeper-side core-модуле `core.soul.registered` (НЕ новый модуль — решение пользователя: консолидация на `registered`, рядом с записью souls+coven, естественна; отдельный `core.soul.online`/барьер-модуль отвергнут как лишняя сущность).

## Способность 1 — onboarding-await (барьер онбординга, S1)

Новые **опциональные** input-поля `core.soul.registered`:

| Поле | Тип | Обяз. | Семантика |
|---|---|---|---|
| `await_online` | bool | — | `true` — после записи souls+coven шаг БЛОКИРУЮЩЕ ждёт онбординга. Default `false` (поведение до ADR не меняется). |
| `await_timeout` | duration | **да при `await_online: true`** | Верхняя граница ожидания. Без него при `await_online: true` — ошибка валидации (барьер не должен висеть вечно). |
| `await_min_count` | int | — | Минимум online-хостов для успеха. Default — **число регистрируемых SID** (все должны подняться). `0 < await_min_count ≤ len(sids)`. |
| `await_poll_interval` | duration | — | Период опроса presence. Default ~2s (parity `acolyte_poll_interval`). |

**Семантика барьера.**

1. Шаг сперва выполняет обычную регистрацию (souls+coven, как до ADR) для **всех** регистрируемых SID.
2. Затем, если `await_online: true`, **блокирующе поллит presence** с периодом `await_poll_interval` под `context.WithTimeout(await_timeout)`, пока число online-хостов среди регистрируемых SID не достигнет `await_min_count`.
3. **Источник истины «online» — Redis SID-lease** (`soul:<sid>:lock` EXISTS, [ADR-006(a)](0006-cache-redis.md), `keeper/internal/redis/SoulsStreamAlive`), **НЕ** PG `souls.status`. PG-статус — lifecycle-снимок, отстаёт; живой EventStream-lease — авторитетный признак, что агент реально на связи (тот же источник, что presence-фильтр таргет-резолвера и lease-aware Reaper).
4. **B1-strict (failure-семантика, решение пользователя).** Если к `await_timeout` online `< await_min_count` — шаг завершается `failed` → fail-stop прогона → `incarnation.state` **не коммитится** → `incarnation.status: error_locked`. Частично-онбордившийся флот не «протекает» в роль-применение: либо набрали кворум, либо явный fail с диагностикой.
5. **output `register.<name>`** дополняется полями барьера: `online: []string` (онлайн SID на момент успеха/таймаута), `pending: []string` (не успевшие), `satisfied: bool` (достигнут ли `await_min_count`). При успехе `satisfied: true`; при failed — событие `failed` + те же поля для диагностики.

**Потолок `await_timeout` (DoS-guard, fail-closed).** Новое поле `keeper.yml::max_await_timeout` (duration). Если шаг задаёт `await_timeout` > потолка — шаг `failed` (а не «тихо обрезали до потолка»: явная ошибка лучше скрытого поведения). Default-потолок — [`DefaultMaxAwaitTimeout`](../../shared/config/keeper.go) (30m). Барьер не должен висеть вечно — это правило защищает кластер от сценария-DoS (зловредный/ошибочный `await_timeout: 100h` держал бы run-goroutine/Acolyte-воркер занятым).

## Способность 2 — mid-run re-resolve roster (S3)

Оживление флага **`refresh_soulprint: true`** на `core.soul.registered` (сейчас заглушка, см. Контекст).

**Семантика.** После **успеха** шага `core.soul.registered` с `refresh_soulprint: true` scenario-runner **пере-резолвит roster инкарнации перед СЛЕДУЮЩИМ Passage**. Онбордившиеся хосты становятся видны последующим шагам: `soulprint.hosts`, `on: [incarnation.name]`, `soulprint.self.*` уже включают актуальный online-набор.

**Re-resolve = live-snapshot (не монотонный рост).** Пере-резолв на refresh-границе — это **свежий live-снимок текущего online-набора инкарнации** (`topology.LoadIncarnationHosts → filterAlive`), а НЕ объединение со старым roster. Это правильная семантика для целевого сценария: роль катится на реально-online хосты.

- Набор **растёт** по мере онбординга провиженных хостов (созданные VM подняли EventStream → видны).
- Набор может и **уменьшиться**: хост P0-roster, ушедший offline к refresh-границе (упал lease / `status≠connected`), из live-снимка **исключается** — на offline-хост роль катить не надо. Это не «удаление хоста из плана», а отражение факта: таргетинг идёт на актуальный online-набор, как и обычный up-front roster.

**Ослабление инварианта стабильности roster (amends [ADR-009 §7](0009-scenario-dsl.md)).** Прежний инвариант — «roster прогона стабилен на весь прогон». Новый — **«roster стабилен в пределах одного Passage; на refresh-границе пере-резолвится (live-снимок)»**. Между Passage roster пере-резолвится, **если** в завершившемся Passage был успешный `refresh_soulprint: true`-шаг.

**Barrier/state-commit-инвариант §7 НЕ ослаблен.** `incarnation.state` по-прежнему коммитится **один раз** после последнего Passage. Re-resolve — ось **roster** (кого таргетить), не ось коммита.

**Реализация re-resolve — S3 (run.go).** Реализован: stage-loop run.go на refresh-границе (`RefreshBoundaries`) вызывает `resolveRoster` (live-снимок) и прокидывает результат в повторный Render следующего Passage; `register.<name>.refreshed` эхает значение флага. Re-resolve fail → abort (не молча на старом roster).

## Stratify — `refresh_soulprint` делает шаг passage-определяющим (S2)

Чтобы re-resolve проявился, потребители обновлённого roster должны оказаться в **следующем** Passage относительно `refresh_soulprint`-шага. Иначе — silent-wrong-target ([ADR-056](0056-staged-render-passage.md): render до dispatch видел бы старый roster).

**Контракт (amends [ADR-056](0056-staged-render-passage.md)).** Введён **НОВЫЙ КЛАСС passage-определяющего ребра — «roster-refresh»**, ОТДЕЛЬНАЯ ось от register-зависимости и program-order: задача `core.soul.registered` с `refresh_soulprint: true` — passage-определяющий эмиттер сигнала «roster-refreshed». Стратификатор ([ADR-056(б)](0056-staged-render-passage.md)) укладывает любого roster-потребителя (`soulprint.hosts` / `soulprint.where(...)` / `on: [incarnation.name]` / опущенный `on:` / `soulprint.self.*`) в Passage **строго ПОСЛЕ** `refresh_soulprint`-шага. Источник этой зависимости — статический признак `refresh_soulprint: true` на задаче (как `register: X` — для probe), но это **не register-граф**: refresh-граница НЕ вводит register-ссылок, поэтому инвариант reads⊆refs ([ADR-056](0056-staged-render-passage.md)) НЕ затрагивается (roster-ось ортогональна register-оси). Семантика re-resolve на этой границе — **live-snapshot** (§S3, не «монотонный рост»). **Реализация — S2** (`shared/config/passage_refresh.go`, ребро вшито в `Stratify`).

## list-SID — регистрация+ожидание N хостов одним шагом (S1)

`core.cloud.provisioned` отдаёт **список** созданных хостов (`register.provision.hosts`). Чтобы зарегистрировать и дождаться их одним шагом-барьером, `core.soul.registered` принимает **список SID**:

- `params.sid` принимает **строку ИЛИ список строк** (runtime, `util.StringOrSliceParam`). Список естественнее одиночного `loop:` — барьер `await_online` агрегирует presence **по всем** SID (нужен общий `await_min_count` поверх набора, а не независимые per-iteration барьеры).
- `params.coven` применяется ко **всем** SID списка (общий набор Coven-меток шага).
- output `register.<name>`: при списочной форме `sid` — массив; при одиночной — строка (историческая форма сохранена). `online`/`pending` — списки SID; `created`/`removed` отражают совокупный side-effect.
- Одиночная строка `sid` остаётся валидной (обратная совместимость) — внутренне нормализуется в список из одного элемента.

**Manifest-DSL trade-off (вскрыт консолидацией, осознанное решение).** Урезанный manifest-input DSL ([`shared/coremanifest`](../../shared/coremanifest/)) **не выражает union `string|list`**, а смена объявленного типа `sid` на `list` сломала бы обратную совместимость одиночно-строковой author-формы (`sid: host.example.com`). Поэтому `sid` объявлен **`type: string`**: одиночная литеральная строка проходит soul-lint как раньше; **список приходит CEL-выражением** `${ register.<step>.hosts }` (основной реальный путь — SID-список из `core.cloud.provisioned`), которое soul-lint пропускает мимо type-check-а независимо от объявленного типа ([ADR-010](0010-templating.md): `${…}`-значение статически не типизируется). **Литеральный список `sid: [a, b]`** соответственно **не проходит статический type-check** soul-lint — приемлемо: на практике SID-список всегда из `register.*` (CEL), а runtime (`StringOrSliceParam`) корректно принимает обе формы. Введение union-типа в manifest-DSL — отдельный ADR (правка публичного контракта), если литеральный список SID понадобится статически.

## HA — provision-сценарии через Voyage

Single-binary провижн-прогон с долгим барьером `await_online` уязвим к крашу инстанса: блокирующий poll держит run-goroutine. **Рекомендация — гнать provision→онбординг→роль сценарии через Voyage** ([ADR-043](0043-voyage.md)), где recovery закрыт ([ADR-027(l)](0027-apply-work-queue.md): осиротевший claim переклеймит другой воркер). Standalone (run-goroutine) staged-recovery для долгого барьера — **открыт** ([ADR-056 §S4](0056-staged-render-passage.md)): при крахе single-keeper run-goroutine во время `await_online`-poll прогон осиротеет в `applying` и потребует ручного разлока (как любой standalone-прогон до Acolyte-cutover). Отмечено в DoD.

## Контракт-сводка

- `core.soul.registered` input расширен: `await_online` (bool), `await_timeout` (duration, required-when await_online), `await_min_count` (int, opt), `await_poll_interval` (duration, opt), `sid` (string **или** list). `refresh_soulprint` (bool) — оживлён.
- `keeper.yml::max_await_timeout` (duration, default 30m) — потолок барьера.
- output `register.<name>` дополнен `online[]` / `pending[]` / `satisfied`.
- presence-источник барьера — Redis SID-lease, не PG.

## Отвергнутые альтернативы

- **Отдельный модуль `core.soul.online` / отдельный барьер-шаг.** Отвергнут (решение пользователя): барьер логически прилегает к регистрации (зарегистрировал созданные SID → дождался их онбординга — одна операция); отдельная сущность — лишний модуль в реестре и в naming-rules без выигрыша.
- **Presence по PG `souls.status`.** Отвергнут: статус отстаёт (lifecycle-снимок), а не реальный online; lease — конструктивно авторитетный признак живого EventStream ([ADR-006(a)](0006-cache-redis.md)). Барьер по PG ложно «увидел» бы хост online до фактического стрима.
- **B0/B2 failure-семантики** (best-effort продолжение при частичном кворуме / warn-без-fail). Отвергнуты в пользу **B1-strict** (решение пользователя): частично-поднятый кластер не должен молча получить роль на неполном наборе — лучше `error_locked` с явной диагностикой `pending[]`.
- **Тихое обрезание `await_timeout` до потолка.** Отвергнуто: явная ошибка `failed` лучше скрытого изменения заявленного поведения.
- **Монотонный рост `roster ∪ newly_online` (только добавление).** Рассматривался, но отвергнут в пользу **live-snapshot**: re-resolve читает свежий online-набор инкарнации. Это правильная семантика для целевого сценария — роль катится на реально-online хосты; хост, ушедший offline к refresh-границе, исключается (катить роль на упавший хост не надо). Объединение со старым roster тащило бы offline-хост в таргетинг. Детерминизм сохраняется: roster стабилен В ПРЕДЕЛАХ Passage, re-resolve только на границах.

## Amends

- **[ADR-009 §7](0009-scenario-dsl.md)** — инвариант стабильности roster ослаблен: «стабилен в пределах Passage; на refresh-границе пере-резолвится (live-снимок)» (не «стабилен на весь прогон»). Barrier/state-commit-инвариант §7 НЕ затронут.
- **[ADR-056](0056-staged-render-passage.md)** — введён новый класс passage-определяющего ребра «roster-refresh» (отдельная ось от register/program-order): `refresh_soulprint: true` делает задачу passage-определяющим эмиттером; roster-потребители стратифицируются в следующий Passage. Refresh-граница не вводит register-ссылок → reads⊆refs не затрагивается.
- **[ADR-006](0006-cache-redis.md)** — Redis SID-lease получает дополнительного потребителя: источник истины барьера онбординга `await_online`.
- **[ADR-017](0017-keeper-side-core.md)** — `core.soul.registered` расширен барьером онбординга + оживлён `refresh_soulprint`; новый config-потолок `keeper.yml::max_await_timeout`.

## DoD

- S1 (этот слайс): `await_online` блокирует на Redis-lease до `await_min_count`/timeout; B1-strict fail; list-SID; потолок `max_await_timeout`; guard-тесты (ждёт→online; B1 timeout→failed; кворум→ok; источник=lease не PG; потолок).
- S2: стратификатор укладывает потребителей roster после `refresh_soulprint`-шага (новый класс ребра «roster-refresh»).
- S3: scenario-runner пере-резолвит roster на refresh-границе (live-snapshot текущего online-набора); `register.<name>.refreshed` эхает флаг.
- no_hosts-bypass (amendment 2026-06-28): два класса provision-from-zero исполняются на пустом roster — (а) all-keeper (`allKeeperTasks`), (б) mixed с refresh-эмиттером (`config.HasRefreshEmitter`, roster пере-резолвится mid-run §S2/§S3); host-only и mixed-БЕЗ-refresh держат no_hosts; Essence keeper-контекст при пустом roster; guard-тесты (keeper-only→исполняет / host-only→no_hosts / mixed+refresh→доходит до dispatch / mixed-без-refresh→no_hosts / юнит `allKeeperTasks` + юнит `HasRefreshEmitter`).
- size-asserts-гейт (amendment 2026-06-29): size-asserts деплой-веток redis (`create/cluster.yml`/`create/sentinel.yml`/`migrate_cluster/cluster.yml`) гейтятся `when: "!(has(input.provision) && input.provision.enabled)"` (STATIC, pre-flight placeholder-skip при provision); roster-инвариант на provision-пути гарантируется `count`-формулой `redis-provision.yml` + `await_online`-барьером, НЕ pre-flight-ом; guard-тесты (provision.enabled+пустой roster→skip не fail / без provision+mismatch→`ErrAssertFailed`); pre-flight остаётся не-staged (правится сервис-DSL, не `EvalAsserts`).
- Открытый риск: standalone staged-recovery долгого барьера ([ADR-056 §S4](0056-staged-render-passage.md)) — provision-сценарии рекомендуется гнать через Voyage.
