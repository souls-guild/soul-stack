# ADR-070. Secret reveal-path — раскрытие plaintext-секрета инкарнации оператору под RBAC-правом

> **Статус: accepted, реализована (NIM-74).** READ-двойник [ADR-064](0064-secret-write-path.md) (secret write-path). ADR-064 принимает plaintext-секрет **ОТ** оператора и пишет его в Vault keeper-side; этот ADR — обратное направление: отдаёт plaintext **ОБРАТНО** оператору под явным правом. **Amends [ADR-064](0064-secret-write-path.md)** (secret-маскинг: санкционированный reveal = снятие маски, не утечка) **и [ADR-047](0047-purview.md)** (новое scoped-право `incarnation.view-secrets`).

**Контекст.** Оператор видит в State-вьюхе, что у инкарнации есть, скажем, redis-пользователи, но не может посмотреть их пароли: `state`/`spec` в GET-ответах маскируются ([ADR-064](0064-secret-write-path.md), defense-in-depth в [operator-api.md](../keeper/operator-api.md)), а сами значения живут в Vault по ref. Для эксплуатации (передать пароль пользователю, проверить подключение руками) оператору нужен **санкционированный** способ раскрыть конкретное значение — не снимая маскинг глобально и не давая прямой доступ в Vault. Механика должна быть **generic** (свойство любого сервиса с секретами в state), а не redis-хардкод в Keeper-е.

Симметрия направлений (тот же plaintext-по-проводу trade-off, что в ADR-064, зеркально):

- **ADR-064 (write):** оператор → plaintext → Keeper → `WriteKV` в Vault → в PG только ref. **Приём** секрета.
- **ADR-070 (read, этот ADR):** оператор → запрос → Keeper → `ReadKV` из Vault → plaintext → оператор. **Отдача** секрета.

## Решение

Декларативный **реестр раскрываемых секретов** в манифесте сервиса + два incarnation-scoped эндпоинта под новым правом.

### Реестр `revealable_secrets` (манифест `service.yml`, generic)

Сервис сам декларирует, ЧТО у его инкарнаций раскрываемо (не redis-хардкод в ядре):

```yaml
revealable_secrets:
  - id: redis-users                                            # адрес декларации (lowercase-идент, уникален)
    label: "Пароли Redis-пользователей"                        # подпись для UI
    enumerate: state.users                                     # state-путь массива объектов; ключ = element.name
    vault_ref: "secret/{service}/{incarnation}/users/{key}#password"
```

- **`id`** — стабильный адрес декларации (lowercase-идент, уникален в манифесте).
- **`label`** — человекочитаемая подпись для UI.
- **`enumerate`** — state-путь массива объектов (форма `state.<segment>`); из имён элементов (`element.name`) собирается **множество допустимых `key`**. В MVP обязателен (см. «Отложенное»).
- **`vault_ref`** — шаблон Vault-пути с плейсхолдерами `{service}` / `{incarnation}` (**оба обязательны** — per-service/per-incarnation-scoping; отсутствие любого → diag `vault_ref_not_service_scoped` на load; см. «Закрытие эскалации») / `{key}` (обязателен при заданном `enumerate`; +опц. `#field` — поле в KV-записи).

### Ограниченные плейсхолдеры (не CEL)

`vault_ref` резолвится **литеральной подстановкой** ровно трёх провалидированных величин — `{service}` (= `inc.Service`, валидно сегмент-паттерном — без `/`/`#`/`..`), `{incarnation}` (= `inc.Name`, валидно по `NamePattern`) и `{key}` (валидно по идент-паттерну **И** обязано ∈ enumerate-массива). Не CEL, не произвольные выражения. Обоснование:

- **меньше attack-surface** — нет вычислимого языка в пути к секрету; чужой путь нельзя сконструировать выражением;
- **анти-произвол** — `key` обязан присутствовать в enumerate-массиве **текущего** state (нельзя раскрыть путь, которого в state нет);
- **анти version-craft** — версия манифеста всегда `incarnation.ServiceVersion` (клиент версию не задаёт; паритет `secretSchemaForIncarnation`);
- **traversal-guard** — резолвленный путь прогоняется через `vault.ParseRef` (режет `..`/`.`/битую форму) ДО чтения; провал → секрет нераскрываем (не 500, путь не палится);
- **позитивный namespace-scoping** — резолвнутый путь обязан лежать под `secret/<service>/<incarnation>/` (**главный** runtime-guard; см. «Закрытие эскалации»).

### Эндпоинты

- **`POST /v1/incarnations/{name}/secrets/reveal`** `{secret_id, key}` → `{value}` — раскрывает одно значение. Self-audit `incarnation.secret_revealed` (факт, БЕЗ значения).
- **`GET /v1/incarnations/{name}/secrets/revealable`** → `{items: [{secret_id, label, state_path, keys}]}` — discovery: что раскрываемо и с какими `key` (READ, без audit). UI строит из этого список; пустой список валиден.

### Санкционированное раскрытие — DTO мимо маскинга

200-тело reveal-эндпоинта несёт plaintext и **НЕ проходит через `MaskSecrets`** — это единственная санкционированная точка выхода значения из домена. Право `incarnation.view-secrets` **и есть** санкция; маскинг остальных sink-ов ([ADR-064](0064-secret-write-path.md)) не трогается.

## RBAC и право

Новое scoped-право **`incarnation.view-secrets`** ([ADR-047](0047-purview.md), каталог — [rbac.md](../keeper/rbac.md#каталог-permissions)):

- **Строго привилегированнее `incarnation.get`** — читать (маскированную) инкарнацию ≠ раскрывать её секреты; отдельное право, не грань `get`.
- **Scope как у incarnation-мутаций** — селекторы `coven=`/`service=`/`incarnation=` по path-`name` (паритет `incarnation.update-hosts`/`incarnation.traits-set`).
- **Fail-closed 404 вне scope** — оператор вне scope получает `404` (parity Get: не палим существование чужой инкарнации), не `403`.
- **MCP — нет (REST-only)** — reveal это UI-действие State-вьюхи (как `form-prefill`), не автоматизируемая операция; MCP-tool не заводится.

## Security trade-off (по образцу [ADR-064 §Security](0064-secret-write-path.md))

Reveal — обратное плечо того же осознанного послабления: plaintext идёт Keeper → оператор по проводу (ADR-064 нёс его оператор → Keeper). Приемлемо ПРИ обязательных митигациях (все — блокеры):

- **(a) RBAC-гейт на инкарнацию** — право `incarnation.view-secrets` + scope-сужение fail-closed (вне scope → 404).
- **(b) Audit факта БЕЗ значения — успех И denied** — `incarnation.secret_revealed` пишет `{name, secret_id, key, path, result, reason}`; **значение секрета в payload НИКОГДА не кладётся**. Аудируется не только успех (`result: "ok"`), но и **каждая denied-ветка ПОСЛЕ резолва инкарнации** (`result: "denied"` + `reason` ∈ `out_of_scope` / `unknown_secret_id` / `key_not_in_state` / `ref_invalid` / `out_of_service_scope` / `floor_denied` / `vault_miss` / `read_error` / `field_missing`) — security-trail на brute-force ключей и попытки чужой инкарнации. (Malformed-request `422` до резолва и несуществующая инкарнация — НЕ аудируются: нечего атрибутировать.) leak-guard-тесты на каждый sink (логи / audit / OTel / текст ошибки).
- **(c) No body-logging** — plaintext покидает домен **только** телом HTTP-ответа (по TLS-транспорту); ни в один лог / текст ошибки не попадает.
- **(d) Ключ-в-state** — `key` обязан ∈ enumerate-массива текущего state (анти-произвол).
- **(e) Traversal-guard** — `vault.ParseRef` над резолвленным путём (режет `..`) + литеральная подстановка вместо CEL.
- **(f) Позитивный namespace-scoping Vault-пути** — резолвнутый путь обязан лежать под `secret/<service>/<incarnation>/` (**главный guard**, runtime) + load-time required `{service}`/`{incarnation}` + floor-backstop; см. ниже «Закрытие эскалации».
- **(g) Vault-policy read-префикс** — Keeper читает секрет своей Vault-policy с read-грантом на детерминированный префикс (`secret/data/redis/*`), не шире.

### Закрытие эскалации «произвольный `vault_ref`»

Манифест сервиса — **не доверенный** вход в модели угроз reveal: автор сервиса (или скомпрометированный service-репо) мог бы задекларировать `vault_ref` на секреты самого Keeper-а (`secret/keeper/jwt-signing-key`, `secret/keeper/sigil-keys/*`), на **чужой service-неймспейс** или на **чужую инкарнацию** — blast-radius = **signing-keys кластера**. Git-review манифеста эту эскалацию НЕ закрывает (одна граница, обходится компрометацией репо). Закрыто **тремя код-уровневыми границами** (defense-in-depth, симметрия с operator-input-каналом `input_vault` — «positive scope-match + unconditional floor»):

1. **Позитивная граница (load-time):** валидация манифеста требует, чтобы `vault_ref` **содержал ОБА плейсхолдера — `{service}` И `{incarnation}`** — путь обязан быть per-service/per-incarnation-scoped; путь без scope-плейсхолдеров (в т.ч. статический keeper-путь) отвергается на load (diag **`vault_ref_not_service_scoped`**).
2. **Позитивный prefix-allowlist (runtime, ГЛАВНЫЙ guard):** резолвнутый logical-путь **обязан начинаться с** `secret/<service>/<incarnation>/` (`<service>` = `inc.Service`, `<incarnation>` = `inc.Name`), иначе denied (`reason: out_of_service_scope`), `404`. **Trailing `/` обязателен** — против prefix-confusion (без него `redis-prod` матчил бы `redis-prod-other`). Проверка **после `vault.ParseRef`, перед `ReadKV`**. Режет **не только** keeper-секреты, но и любой чужой service-неймспейс и чужую инкарнацию: reveal физически читает ТОЛЬКО под неймспейсом секретов своей инкарнации своего сервиса. (`inc.Service` перед подстановкой валидируется сегмент-паттерном — анти-инъекция `/`/`#`/`..`.)
3. **Floor-backstop (runtime):** `config.DeniedByVaultFloor` (неотключаемый system-floor `secret/keeper/` / `secret/internal/`, тот же, что у канала `input_vault`) безусловно перед `ReadKV` (`reason: floor_denied`) — страховка для **edge-case сервиса с зарезервированным именем** `keeper`/`internal` (для такого сервиса позитивный allowlist дал бы `secret/keeper/<inc>/` — floor его добивает).

## Отвергнутые альтернативы

- **Redis-специфичный хардкод-эндпоинт** (`GET .../redis/users/{u}/password`). Отвергнут в пользу generic-реестра `revealable_secrets`: reveal — свойство любого сервиса с секретами в state; зашивать redis в ядро — тупик (каждый новый класс сервиса = правка Keeper-а).
- **CEL в `vault_ref`.** Отвергнут в пользу ограниченных плейсхолдеров `{service}`/`{incarnation}`/`{key}`: вычислимый язык в пути к секрету — лишний attack-surface без выигрыша (реальные пути параметризуются ровно сервисом, инкарнацией и ключом).
- **Переиспользование `incarnation.get`.** Отвергнуто: reveal **строго** привилегированнее чтения — снятие маскинга должно требовать отдельного явного права, иначе любой читатель инкарнации автоматически видит её пароли.
- **Git-review манифеста как единственная граница.** Отвергнуто: полагаться только на code-review секции `revealable_secrets` (что автор не впишет keeper-путь) недостаточно — blast-radius эскалации = **signing-keys кластера**, а манифест — не доверенный вход. **Code-level guard обязателен** (позитивный prefix-allowlist `secret/<service>/<incarnation>/` + required-`{service}`/`{incarnation}` + floor-backstop `DeniedByVaultFloor`; см. «Закрытие эскалации»), git-review — дополнительный, не заменяющий слой.

## Отложенное (post-MVP, без breaking changes)

- **Singleton-секреты без `enumerate`** — секрет-одиночка на инкарнацию (напр. admin-пароль `secret/{service}/{incarnation}#password`), где перечислять нечего (нет массива, `key` не нужен). MVP требует `enumerate` (форма коллекции); одиночки — аддитивное расширение (`enumerate` опционально + reveal без `key`; `{service}`/`{incarnation}` остаются обязательными) при реальном запросе.
- **Живой манифест `community.redis`** несёт секцию `revealable_secrets` — правка в репозитории модуля (follow-up вне core-репо).
- **Проброс config-extra-deny в reveal-handler.** Сейчас floor — только system-floor (`DeniedByVaultFloor(logical, nil)`); операторский `keeper.yml → vault.input_deny_paths` (доп. deny-префиксы, уже действующие для канала `input_vault`) в reveal пока НЕ пробрасывается — follow-up.
- **Аудит RBAC-403 на gate-уровне.** Denied-ветки аудирует handler ПОСЛЕ резолва инкарнации; отказ на middleware-gate (`incarnation.view-secrets` не держится вовсе → `403` ДО handler-а) этим событием не аудируется — кросс-каттинг, общий для всех роутов; follow-up отдельным решением.

## Impact (реализация — NIM-74, вне этого ADR)

`shared/config` (парс + валидация `revealable_secrets[]`) + Operator API 2 эндпоинта (+OpenAPI drift-regen) + reveal-handler (scope-гейт + enumerate-guard + `vault.ParseRef` + позитивный namespace-allowlist `secret/<service>/<incarnation>/` + floor-backstop + `ReadKV`) + audit-event + RBAC-право (каталог [rbac.md](../keeper/rbac.md#каталог-permissions)) + vault-policy read-префикс + companion UI (State-вьюха, reveal-контрол) + leak-guard-тесты. **Реализовано в NIM-74.**

## Связь с ADR

- **[ADR-064](0064-secret-write-path.md)** — secret write-path (приём plaintext-а ОТ оператора). Этот ADR — READ-двойник (отдача plaintext-а ОБРАТНО); **amends** его secret-маскинг (санкционированный reveal = снятие маски, не утечка).
- **[ADR-047](0047-purview.md)** — Purview scoped-RBAC; **amends** новым правом `incarnation.view-secrets` (scope как incarnation-мутации, fail-closed 404 вне scope).
- **[ADR-053](0053-dependency-tiers.md)** — Vault hard-required (reveal читает из него).
- **[ADR-022](0022-audit-pipeline.md)** — audit-pipeline (событие `incarnation.secret_revealed`).
