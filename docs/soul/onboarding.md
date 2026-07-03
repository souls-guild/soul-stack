# Онбординг Soul: bootstrap-токен и доставка

Применимо к pull-режиму (`transport: agent`). Для push-режима (`transport: ssh`) онбординг сводится к настройке SSH-доступа и не использует bootstrap-токен — см. [architecture.md → Push-режим](../architecture.md#push-режим-keeperpush).

## Жизненный цикл bootstrap-токена

«Сжигание» — операция в две стороны, обе атомарные.

### На стороне Keeper

При предъявлении токена и CSR — одной транзакцией Postgres:

```sql
UPDATE bootstrap_tokens
   SET used_at     = now(),
       used_by_kid = $self_kid
 WHERE token_hash  = $hash_of_presented
   AND sid         = $claimed_sid
   AND used_at     IS NULL
   AND expires_at  > now()
RETURNING token_id;
```

- `UPDATE … WHERE used_at IS NULL` + row-level lock делает операцию race-safe: два одновременных предъявления одного токена дадут один успех и один отказ.
- Пустой `RETURNING` — токен уже сожжён, истёк или не существует → `403`, `souls.status` остаётся `pending`.
- В той же транзакции: `souls.status: pending → connected`, в `soul_seeds` создаётся `active`-запись с подписанным сертификатом. Полу-состояние «токен сожжён, но seed не создан» невозможно.
- Plain-токен Keeper не хранит — после выписки оператору он уходит из памяти. В БД — только `token_hash` (SHA-256, hex, без соли — токен уже сам по себе high-entropy).

### На стороне Soul

- `soul init` получает токен **строкой** — из флага `--token` или env `SOUL_BOOTSTRAP_TOKEN` (см. [§ Поток онбординга](#поток-онбординга-агентский-режим)); файлов с токеном сам бинарь **не читает и не удаляет**. Один процесс `init` = одно предъявление; после успешного bootstrap токен уже сожжён на стороне Keeper (SQL-транзакция выше) и повторно не пригоден.
- Если канал доставки клал токен в файл (например, `/etc/soul/token` у `core.bootstrap.delivered`, [ADR-063](../adr/0063-bootstrap-token-delivery.md)) — файл остаётся артефактом **канала доставки**, его гигиена (права `0400`, чистка) — на канале/операторе, не на `soul`-бинаре. Основные защиты — одноразовость и короткий TTL токена, а не перезапись диска.
- Содержимое токена **никогда** не логируется — ни `soul`-ом, ни Keeper-ом (на выходах Keeper-а ключ `bootstrap_token` маскируется `audit.MaskSecrets`).

### Рекомендации оператору

Вне зоны ответственности Soul Stack, но влияет на безопасность онбординга:

- Файл токена — `mode 0400`, owner `soul:soul`, директория `mode 0700`.
- На systemd ≥ 250 — использовать `LoadCredential=` в unit-файле. Токен живёт в tmpfs, передаётся в процесс через ephemeral fd, **никогда не пишется на диск**. Лучшее решение, всё остальное — компромисс.

**Жнец** позже подбирает использованные токены: правило `purge_used_tokens` с `max_age: 90d` от `used_at` — это уже не про безопасность, а GC. См. [architecture.md → Reaper / Жнец](../architecture.md#reaper--жнец).

## Поток онбординга (агентский режим)

1. Оператор (или скрипт) через OpenAPI/MCP `keeper`-а регистрирует хост и получает bootstrap-токен под конкретный SID:
   - **Первичная регистрация** — `POST /v1/souls` (permission `soul.create`, MCP-tool `keeper.soul.create`, [operator-api.md](../keeper/operator-api/souls.md#post-v1souls--зарегистрировать-soul)). Запись в `souls` появляется в статусе `pending`, `requested_at = now()`, `created_by_aid` = вызвавший Архонт (FK на `operators(aid)`). Для `transport: agent` в той же операции выпускается bootstrap-токен — одноразовый, TTL по умолчанию 24h; plain-токен в ответе один раз.
   - **Повторный выпуск** — `POST /v1/souls/{sid}/issue-token` (permission `soul.issue-token`, MCP-tool `keeper.soul.issue-token`). Используется при потере токена или плановой ре-выписке для уже существующей Soul. Подробности — [§ Восстановление: потерян токен](#восстановление-потерян-токен).
2. Доставка `soul`-бинаря, готового конфига и SoulSeed-токена на хост — задача оператора. Допустимые механизмы см. в разделе «Способы доставки токена» ниже.
3. Оператор однократно выполняет `soul init [--config /etc/soul/soul.yml]`. Команда:
   - берёт токен из флага `--token=<токен>` **или** из env `SOUL_BOOTSTRAP_TOKEN` (флаг побеждает env; оба пусты → ошибка). **stdin не читается.** Env-форма предпочтительнее: значение `--token` светится в `ps` и shell-history, env — нет;
   - endpoints, retry, tls берёт из конфига (`keeper.endpoints` и т.д.); из аргументов командной строки — только `--token`, `--config` и `--sid` (override SID: `--sid` > `sid:` конфига > `os.Hostname` lowercased);
   - локально генерирует приватный ключ (никогда не покидает хост) и CSR с SID = FQDN;
   - подключается к Keeper Bootstrap-listener-у на `endpoints[].bootstrap_port` (server-only TLS), перебирая endpoints по `priority` от меньшего к большему без in-group shuffle (one-shot, порядок детерминирован; spray/shuffle есть только в EventStream-фазе — см. [connection.md → Две фазы, два порта](connection.md#две-фазы-два-порта)), предъявляет токен + CSR;
   - получив подписанный сертификат, атомарно раскладывает SoulSeed в `paths.seed` и завершается.

   Если SoulSeed уже лежит на диске — `init` падает с ошибкой, чтобы случайно не перевыпустить (для перевыпуска — отдельная процедура, см. [identity.md → Ротация SoulSeed](identity.md#ротация-soulseed)).
4. Keeper при предъявлении токена и CSR:
   - проверяет, что токен валиден, не истёк, не использован, и SID совпадает;
   - выпускает SoulSeed (mTLS-сертификат и ключ через Vault PKI / встроенный CA — конкретная реализация зафиксирована в ADR-006/Vault-разделе);
   - возвращает Soul-у;
   - помечает токен использованным (SQL-транзакция выше);
   - переводит запись в `souls` в `connected`, заполняет поля seed-а.
5. Дальше — обычный запуск демона `soul` (через systemd-unit и т.п.); он держит стрим по алгоритму из [connection.md](connection.md).

## Восстановление: потерян токен

Plain bootstrap-токен Keeper хранит только до выписки оператору — в БД остаётся лишь `token_hash` ([§ На стороне Keeper](#на-стороне-keeper)). Потерянный токен восстановить нельзя, только выпустить новый.

Recovery-flow для существующей Soul (`transport: agent`), которая ещё не прошла онбординг (`status: pending`):

1. Оператор с permission `soul.issue-token` вызывает `POST /v1/souls/{sid}/issue-token` (CLI/обёртка — `--force` при наличии активного токена; MCP — `force: true`).
2. Действует инвариант `UNIQUE (sid) WHERE used_at IS NULL` — максимум один активный токен на Soul:
   - **без `force`** при уже-активном токене → `409 bootstrap-token-active` (защита от плодящихся валидных токенов);
   - **с `force=true`** → старый активный токен помечается использованным (`used_at = now()` — освобождает partial-unique slot `WHERE used_at IS NULL`), выпускается новый.
3. Новый plain-токен доставляется на хост обычным способом ([§ Способы доставки токена](#способы-доставки-токена)), `soul init` повторяется.

Для Soul с `transport: ssh` `issue-token` отдаёт `422 validation-failed` — ssh-онбординг не использует bootstrap-токен ([architecture.md → Push-режим](../architecture.md#push-режим-keeperpush)).

## Способы доставки токена

«Оператор сгенерировал bootstrap-токен → токен оказался в файле на ВМ, где запустится `soul`» — для этого допускаются разные пути. Часть из них — **внутри Soul Stack** (через `keeper.push`), часть — **вне зоны ответственности** (внешние тулзы оператора). Soul Stack принимает все варианты, потому что выписка токена и его приём — это API/MCP операции; способ физической доставки — выбор оператора.

- **Через шаг сценария `core.bootstrap.delivered` (целевой вариант, внутри Soul Stack, [ADR-063](../adr/0063-bootstrap-token-delivery.md)).** Keeper-side core-модуль доставки: по SSH кладёт per-VM токен в файл на хосте (`token_path`, default `/etc/soul/token`; токен идёт через STDIN, не argv), там же делает redeem — `test -e <seed-cert> || SOUL_BOOTSTRAP_TOKEN="$(cat <token_path>)" soul init` — и опционально активирует unit (`daemon-reload && enable && start`). Два режима: **token-only** (setup уже поставил cloud-init) и **full-install** (весь setup — CA/soul.yml/unit/бинарь — тем же SSH-каналом, для платформ без cloud-init userdata). Типичное применение — после `core.cloud.created` в provision-сценарии ([keeper/cloud.md](../keeper/cloud.md)); спецификация модуля — [keeper/modules.md → core.bootstrap.delivered](../keeper/modules.md#corebootstrapdelivered). Преимущество: единый аудит, RBAC и логи в Keeper-е, никаких сторонних тулз.
- **Ansible-role.** Рекомендуемая официальная роль будет жить в отдельном репозитории; принимает токен и адрес Keeper-а как переменные. Хорошо для тех, у кого Ansible — корпоративный стандарт.
- **Обычный SSH/SCP** — оператор кладёт токен и `soul`-бинарь вручную или своим скриптом.
- **CI/CD pipelines** — токен берётся из CI-secret store, доставляется через terraform-provisioner или bootstrap-скрипт.
- **Cloud-init / image baking** — для эфемерных VM, где токен подкладывается на этапе создания инстанса.

## Защиты со стороны Soul Stack

- **TTL токена** — короткий по умолчанию (24h), настраивается оператором.
- **Одноразовость** — токен сжигается при первом успешном CSR (SQL-транзакция выше).
- **Привязка к конкретному SID** — токен валиден только для того FQDN, под который выписан.
- **Аудит** — каждая выписка и использование токена логируются в Keeper-е.

Дополнительные защиты (привязка к IP/CIDR, требование cloud-metadata-доказательства, ручное approval) — см. open-вопрос «Утечка SoulSeed-токенов» в [architecture.md → Открытые вопросы](../architecture.md#открытые-вопросы).

## См. также

- [identity.md](identity.md) — реестр `bootstrap_tokens`, реестр `soul_seeds`, статусы Soul.
- [connection.md](connection.md) — алгоритм, по которому `soul init` подключается к Keeper-у.
- [config.md](config.md) — где на хосте `paths.seed` и `tls.ca`.
- [architecture.md → Жизненный цикл Soul и реестр душ](../architecture.md#жизненный-цикл-soul-и-реестр-душ) — архитектурный обзор онбординга и реестров.
- [architecture.md → Доставка SoulSeed-токена на хост](../architecture.md#доставка-soulseed-токена-на-хост) — короткое перечисление способов доставки.
