# ADR-050. Tempo — per-AID rate-limiting write-API

> **Статус: active.** Имя **Tempo** выбрано пользователем (propose-and-wait пройден); дизайн — architect. Реализация (Redis-примитив token-bucket, middleware, problem-type, config-блок + wire + guard-тесты) выполнена (слайсы S-R1..S-R4).

**Amendment (2026-06-17, отдельный bucket `voyage_preview` — дизайн architect, вариант a).** До этого `POST /v1/voyages/preview` реюзил bucket `voyage_create` (единый лимит create+preview). Найденная проблема: preview и create **делили одну per-AID квоту** — частые preview (предпоказ числа батчей в UI при late-binding-таргете) съедали бюджет реального create, и наоборот. **Решение:** preview получает **собственный bucket `voyage_preview`** с собственными rate/burst. Дефолты — **`rate: 30 rps, burst: 60`** (мягче create `10/20`). Обоснование асимметрии: preview **read-like по эффекту** (без INSERT в `voyages`/`voyage_targets`, без audit-записи), поэтому заслуживает более мягкого лимита; но preview **resolver-heavy по стоимости** (тот же Purview-резолв scope + page-CEL по флоту, что у create) → **не безлимит**, а отдельный, более широкий потолок. Per-AID Redis-ключи `tempo:<aid>:voyage_create` и `tempo:<aid>:voyage_preview` независимы (форма ключа из (a) уже это обеспечивает — разные bucket-имена дают разные ключи). Forward-compat additive: новое поле `tempo.voyage_preview.{rate,burst}` в конфиге, опущение → дефолт; никаких breaking changes. Зафиксировано в (c)/(e)/(f).

**Контекст.** Resolver-тяжёлые write-эндпоинты Operator API (`POST /v1/voyages` — резолв scope-границы оператора через [Purview](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор), пересечение с invocation-target, page-CEL по soulprint/state на 100k-флоте) дороги по своей природе. Два anti-DoS-слоя уже есть: **body-limit** (отсечь негабаритное тело до парсинга) и **[Toll](0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls)** (cluster-wide блок write-API при массовом оттоке Souls). Не покрыт **third vector** — отдельный аутентифицированный Архонт (свой `claims.Subject` / AID), молотящий resolver-тяжёлый create в цикле: тело валидно (body-limit пропускает), кластер здоров (Toll не взведён), но resolver-нагрузка от одного оператора кладёт инстанс. Нужен **per-AID ограничитель частоты** обращений к этим эндпоинтам — третий anti-DoS-слой.

**Решение.** Вводится сущность **Tempo** — сквозной per-AID ограничитель частоты запросов оператора к resolver-тяжёлым write-эндпоинтам. Срабатывает после аутентификации (известен `claims.Subject` = AID) и до handler-а (до запуска резолверов). Метафора — музыкальная линия рядом с [Conductor](../architecture.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)/[Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)/[Choir](0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации): «допустимый темп обращений оператора к API». Граница с Cadence: Cadence решает **когда спавнить** Voyage (расписание), Tempo — **как часто оператор дёргает API** (rate-limit запросов).

**(a) Redis-backend — авторитет лимита.** Token-bucket живёт в Redis (hash `{tokens, last_refill_ts}` + `PEXPIRE`), ключ **`tempo:<aid>:<bucket>`** (per-AID, per-логический-bucket эндпоинта). Refill+take — **атомарно одним Lua-скриптом** (read-modify-write бакета в одном round-trip, без race между инстансами). **In-memory per-инстанс ОТВЕРГНУТ:** при stateless-HA ([ADR-002](0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)) лимит размножился бы ×N инстансов (10 rps на инстанс × N = N×10 rps на оператора) и зависел бы от LB-распределения — некогерентен. Redis — общий горячий слой кластера ([ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)), естественный авторитет.

**(b) fail-OPEN при Redis-down — осознанный security-trade-off.** Если Redis недоступен (скрипт упал / connection refused), Tempo **выключается (passthrough)** — запрос проходит без rate-check. Это **то же поведение, что у [Toll](0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls)** (Toll при сбое Redis не взводит degraded). **Зафиксировано как осознанный security-trade-off, принято пользователем (2026-06-09): доступность > перестраховка.** Fail-closed (503 при недоступном Redis) **отвергнут** — отказ Redis заблокировал бы весь resolver-тяжёлый write-API кластера, превратив сбой горячего слоя в полный отказ управляющей плоскости; rate-limit — защита от abuse, не safety-критичный gate, его временное отключение допустимо. NB: это **противоположно** fail-closed-инварианту scoped-видимости ([ADR-047 S3b](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор), при сомнении скрывает) — там неопределённость scope = утечка данных, здесь неопределённость rate = временная потеря throttle; разные риски, разные стратегии.

**(c) Охват MVP.** **`POST /v1/voyages`** (create, bucket `voyage_create`) + **`POST /v1/voyages/preview`** (dry-resolve scope, bucket `voyage_preview`). **Каждый путь — СВОЙ bucket** (см. amendment 2026-06-17 ниже): per-AID Redis-ключи `tempo:<aid>:voyage_create` и `tempo:<aid>:voyage_preview` независимы — исчерпание одного не throttle-ит другой. Прочие write-эндпоинты под Tempo — **additive позже** (новый bucket в конфиге + навеска middleware, без breaking change). Read-API под Tempo не ставится (дёшев, не resolver-тяжёл).

**(d) Превышение — 429 + Retry-After + problem+json.** При исчерпании бакета — **HTTP 429** с заголовком `Retry-After` (секунды до пополнения хотя бы одного токена) и телом `application/problem+json` (RFC 7807, problem-type **`tempo-exceeded`** — symbolic `TypeTempoExceeded`). Единый формат с [Toll](0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls)-503 (тот же `Retry-After`-паттерн, тот же problem+json-каркас — [operator-api.md → Error format](../keeper/operator-api.md#error-format-rfc-7807)), разный код и type: Toll = 503 cluster-degraded, Tempo = 429 per-AID-rate.

**(e) Дефолты.** Per-AID, по bucket-у:
- `voyage_create`: **`rate: 10 rps, burst: 20`**;
- `voyage_preview`: **`rate: 30 rps, burst: 60`** (мягче create — preview read-like по эффекту, без persist/audit — но НЕ безлимит: dry-resolve так же resolver-heavy; см. amendment 2026-06-17 ниже).

Burst — глубина бакета, rate — скорость refill. Подобраны как «человеку/нормальному автоматону хватает с запасом, цикл-abuse режется».

**(f) Config `tempo:`** ([ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml), hot-reload):

```yaml
tempo:
  enabled: true            # default-ON при наличии Redis (footgun-guard, как Conductor/Toll)
  voyage_create:
    rate: 10               # rps, refill-скорость бакета
    burst: 20              # глубина бакета
  voyage_preview:          # ОТДЕЛЬНЫЙ bucket (amendment 2026-06-17), не делит квоту с create
    rate: 30               # rps, мягче create (preview read-like, но resolver-heavy)
    burst: 60              # глубина бакета
```

`enabled` / `voyage_create.{rate,burst}` / `voyage_preview.{rate,burst}` — hot-reloadable (atomic swap полей; новый лимит применяется со следующего запроса, текущие бакеты в Redis доживают по своему `PEXPIRE`). Опущение любого `voyage_*`-блока / поля → дефолт из (e). Нормативная типизация блока — [`docs/keeper/config.md → tempo`](../keeper/config.md#tempo) (docs-writer при имплементации).

**(g) Метрики.** `keeper_tempo_allowed_total{endpoint}` / `keeper_tempo_rejected_total{endpoint}` (counter). Лейбл `endpoint` (= bucket-имя, `voyage_create`); **AID-лейбла НЕТ** — кардинальность (число операторов не ограничено, AID в лейбле взорвал бы time-series). Кто именно превышает — видно в audit/логах по `claims.Subject`, не в метриках.

**Обоснование.**
- **Третий anti-DoS-слой, ортогональный первым двум.** body-limit режет по размеру тела, Toll — по здоровью кластера (cluster-wide), Tempo — по частоте per-AID. Три независимых вектора, не дублируют друг друга.
- **Соответствие [ADR-002](0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)/[ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis).** Redis-backend когерентен в stateless-HA; никакой новой инфраструктуры — переиспользование горячего Redis-слоя.
- **Безопасность на первом месте, но availability-first на сбое горячего слоя (b).** Trade-off зафиксирован явно, не «деталь имплементации».

**Consequences.**
- **Middleware** на Operator API (после auth, до handler-а) для эндпоинтов в охвате (c).
- **Redis-примитив** token-bucket (Lua, ключ `tempo:<aid>:<bucket>`) — `shared/`/`keeper/internal` (раскладка — слайс S-R1).
- **problem-type `tempo-exceeded`** (`TypeTempoExceeded`) — [naming-rules.md → Error codes](../naming-rules.md#error-codes), каталог [operator-api.md → Error format](../keeper/operator-api.md#error-format-rfc-7807) (docs-writer).
- **Config-блок `tempo:`** — [`docs/keeper/config.md`](../keeper/config.md) (docs-writer).
- **Метрики** `keeper_tempo_allowed_total` / `keeper_tempo_rejected_total` — [naming-rules.md → Метрики](../naming-rules.md).
- **OpenAPI** — добавить ответ `429` (`application/problem+json` + `Retry-After`) к `POST /v1/voyages` и `POST /v1/voyages/preview` (docs-writer / S-R4).

**Связь с ADR.**
- **[ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)** — `POST /v1/voyages` (bucket `voyage_create`) + `/v1/voyages/preview` (bucket `voyage_preview`) — единственный охват MVP; preview — ОТДЕЛЬНЫЙ bucket (amendment 2026-06-17), не делит квоту с create.
- **[ADR-038](0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls)** — соседний anti-DoS-слой (cluster-wide write-block), НЕ конфликт: Toll = 503 по здоровью кластера, Tempo = 429 per-AID по частоте; единый problem+json/`Retry-After`-каркас.
- **[ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)** — Redis-backend (token-bucket).
- **[ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)** — config-блок `tempo:` hot-reloadable.
- **[ADR-047](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)** — resolver-тяжесть voyage-create (Purview-резолв scope) — мотив лимита.

**Отвергнутые альтернативы.**
- **(а) In-memory per-инстанс rate-limit.** Лимит ×N инстансов, зависимость от LB-распределения — некогерентен в stateless-HA. Отвергнут (a).
- **(б) fail-closed (503 при Redis-down).** Отказ горячего слоя заблокировал бы весь write-API. Отвергнут (b).
- **(в) AID-лейбл в метриках.** Кардинальность — number-of-operators не ограничен. Отвергнут (g).

**Slice-план R.** S-R0 — канон (этот ADR). S-R1 — Redis-примитив token-bucket (Lua + ключ `tempo:<aid>:<bucket>`). S-R2 — middleware + problem-type `tempo-exceeded`. S-R3 — config-блок `tempo:` + wire + навеска на router (`POST /v1/voyages`). S-R4 — guard-тесты (rate/burst/fail-open/per-AID-изоляция) + OpenAPI-ответ 429. **Amendment 2026-06-17** — отдельный bucket `voyage_preview` (30/60): config-поле `tempo.voyage_preview`, перевеска preview-роута, guard-тесты «create и preview не делят квоту».
