# Идентичность Soul и реестры

Архитектурный обзор и связь с другими разделами — в [architecture.md → Жизненный цикл Soul и реестр душ](../architecture.md#жизненный-цикл-soul-и-реестр-душ). Здесь — собранная картинка с точки зрения Soul-а: «кто я в БД Keeper-а и какими полями я там описан».

## Идентичность

- **SID — Soul ID = FQDN хоста.** Не UUID. Это автоматически даёт дедуп при переустановке агента на тот же хост, ценой того, что переименование FQDN = миграция (см. «Переименование хоста» ниже).
- **KID — Keeper ID.** Стабильный идентификатор Keeper-инстанса. Появляется в полях `last_seen_by_kid`, `used_by_kid`, `issued_by_kid`.
- **SoulSeed — единый артефакт идентичности Soul.** Пара (mTLS-сертификат + приватный ключ), которой Soul аутентифицируется при подключении к Keeper. Soul при первом запуске запрашивает у Keeper «дай семя», получает SoulSeed, раскладывает на хосте. Дальше — регулярные ротации (раз в неделю): Soul по живому стриму просит новый SoulSeed, Keeper выпускает, старый помечается `superseded`.
- **Coven — метка группы Soul.** Произвольная метка/тег для логического объединения Souls (по ЦОДу, по роли, по окружению). Используется в RBAC, таргетинге Destiny и потенциально в маршрутизации балансировщика. Реальная маршрутизация по Coven — открытый вопрос (`LB-1`).

Приватный ключ SoulSeed **никогда не покидает хост**. Он генерируется локально на этапе [онбординга](onboarding.md), используется для подписи CSR, и дальше живёт в `paths.seed` рядом с подписанным сертификатом. У Keeper-а в БД хранится только fingerprint, не PEM и не ключ.

## Реестр `souls`

Таблица в Postgres со следующими полями (упрощённая схема, без операционных индексов):

| поле | тип | смысл |
|---|---|---|
| `sid` | text PK | FQDN хоста |
| `transport` | enum | `agent` \| `ssh` — как Keeper доставляет команды |
| `status` | enum | `pending` \| `connected` \| `disconnected` \| `revoked` \| `expired` \| `destroyed` (см. ниже) |
| `coven` | text\[\] | метки группы |
| `registered_at` | timestamptz | когда душа впервые принята |
| `last_seen_at` | timestamptz | время последнего успешного чека (актуальное значение в Redis, в PG — flush) |
| `last_seen_by_kid` | text | какой Keeper последним держал стрим |
| `created_by_aid` | text FK → `operators(aid)` | кто завёл душу |
| `requested_at` | timestamptz | когда оператор выписал SoulSeed-токен |
| `note` | text | свободное поле оператора |

## Реестр `bootstrap_tokens`

Отдельная таблица: на одну Soul может быть **один активный (неиспользованный) токен** одновременно. После использования запись остаётся в таблице для аудита, очищается Жнецом (см. [architecture.md → Reaper / Жнец](../architecture.md#reaper--жнец)).

Для push-хостов (`transport: ssh`) записи в `bootstrap_tokens` **не создаются** — у них нет bootstrap-фазы, симметрично с `soul_seeds` (см. ниже).

| поле | тип | смысл |
|---|---|---|
| `token_id` | UUID PK | первичный ключ записи |
| `sid` | text FK → `souls.sid` | под какую Soul выпущен |
| `token_hash` | text | SHA-256 от plain-токена в hex; **сам plain-токен в БД не хранится** |
| `created_at` | timestamptz | когда выписан |
| `expires_at` | timestamptz | TTL по умолчанию `created_at + 24h` |
| `used_at` | timestamptz NULL | когда сожжён (NULL = ещё активен) |
| `used_by_kid` | text NULL | какой Keeper принял предъявление |
| `created_by_aid` | text FK → `operators(aid)` | кто выписал |

**Инвариант:** `UNIQUE (sid) WHERE used_at IS NULL` — на одну Soul одновременно может быть только один неиспользованный токен.

**Cascade при cloud-destroy ([ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)):** если SID удаляется через `core.cloud.provisioned destroyed`, ещё-не-использованные токены этого SID помечаются `used_at = NOW()`, `used_by_kid = 'system-cloud-destroy'` (специальный маркер, **не** реальный KID и **не** AID; защищает анти-replay-инвариант).

Жизненный цикл (выписка → доставка → CSR → сжигание) и SQL-транзакция предъявления — в [onboarding.md](onboarding.md).

## Реестр `soul_seeds`

Отдельная таблица: на один SID — много seed-ов (история ротаций). Один активный одновременно.

| поле | тип | смысл |
|---|---|---|
| `seed_id` | UUID PK | первичный ключ |
| `sid` | text FK → `souls.sid` | владелец |
| `fingerprint` | text | SHA-256 публичного ключа сертификата, hex (без HMAC, без соли) |
| `serial_number` | text | серийник сертификата |
| `issued_at` | timestamptz | когда выдан |
| `expires_at` | timestamptz | когда истекает |
| `issued_by_kid` | text | какой Keeper выдал |
| `status` | enum | `active` \| `superseded` \| `expired` \| `revoked` \| `orphaned` (см. ниже) |
| `revocation_reason` | text NULL | если revoked — почему |

**Инварианты:**
- `UNIQUE (sid) WHERE status='active'` — ровно один активный seed на SID.
- В БД **не хранятся** PEM, приватные ключи, и отдельный публичный ключ — только fingerprint. Главная защита — приватный ключ CA в Vault.

**Статусы:**
- `active` — текущий выпущенный сертификат, ровно один per SID.
- `superseded` — заменён ротацией, новый seed уже active.
- `expired` — двинут Жнецом / Vault PKI после `not_after`.
- `revoked` — оператор отозвал (security-инцидент, compromise). Audit-семантика: «оператор принял решение».
- `orphaned` — хост cascade-удалён из `core.cloud.provisioned destroyed` ([ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)). Audit-семантика: «жизненный цикл VM завершился». Cascade применяется только к `active`-seed-ам; `revoked` НЕ перетирается (precedence revoked > orphaned).

Для push-хостов (`transport: ssh`) `soul_seeds` **не используется** — у них нет mTLS-идентичности, см. [concept.md → Два транспорта](concept.md#два-транспорта).

## Статусы Soul и переходы

- **`pending`** — оператор выписал SoulSeed-токен под этот SID, Soul ещё не пришёл.
- **`connected`** — legacy lifecycle-снимок «последнее известное: стрим был жив». **НЕ источник presence** (см. ниже): online/offline решает Redis SID-lease, не этот статус.
- **`disconnected`** — legacy lifecycle-снимок «последнее известное: стрим был закрыт/потерян». Soul может вернуться; presence (online) при этом восстановится через захват lease, независимо от того, успел ли Жнец согласовать снимок обратно в `connected`.
- **`revoked`** — оператор отозвал. Сертификат в `soul_seeds` помечается `revoked`, новые подключения от этого SID отвергаются на TLS-уровне.
- **`expired`** — Жнец передвинул `pending` после TTL bootstrap-токена (Soul так и не пришёл).
- **`destroyed`** — terminal-state ([ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) cascade): хост физически удалён через `core.cloud.provisioned destroyed`. Исходящих переходов нет — запись остаётся как forensic-объект и **не входит** в default-set `purge_souls.statuses` ([keeper/reaper.md](../keeper/reaper.md)). Оператор может удалить вручную.

### Presence (online/offline) = Redis SID-lease, не `souls.status`

**Авторитет «Soul online» — живой Redis SID-lease** `soul:<sid>:lock` (значение = `kid` владеющего Keeper-инстанса, [ADR-006(a)/(b)](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)). Lease захватывается на EventStream session-open (после handshake) и продлевается renewal-goroutine-ой, пока стрим жив; гаснет на штатном Release (teardown) либо по TTL после crash-а инстанса. **Синхронной записи presence в `souls.status` на connect/disconnect НЕТ** — на цель 100k VM это был бы hot-path PG-записей; presence держится в горячем Redis-слое.

**Таргет-резолвер прогона деривирует online из lease**, не из `souls.status`: двухфазно — (1) SQL-кандидаты по Coven-членству + status НЕ terminal/онбординг (`pending`/`revoked`/`expired`/`destroyed` исключены), (2) отсев кандидатов без живого SID-lease (batch EXISTS). Так переподключившийся после рестарта Keeper-а Soul виден резолверу сразу по факту захвата lease — даже если снимок `souls.status` ещё `disconnected`. Idle-Soul (шлёт лишь soulprint раз в `refresh_interval`, ни одного app-сообщения в окне) остаётся online, пока renewal держит lease. Без настроенного Redis (single-instance dev / unit) резолвер деградирует на SQL-снимок (`status='connected'`).

**Что пишет `souls.status`** (lifecycle-снимок для Operator API «последнее известное», НЕ presence):

- **Bootstrap-RPC** (онбординг): `pending` → `connected`, фиксирует `last_seen_by_kid`. Одна запись при онбординге, не hot-path. Реконнект уже-онбордированного Soul-а Bootstrap-RPC **не** трогает (онбординг уже пройден) — снимок обратно в `connected` двигает Жнец-reconcile (ниже).
- **Жнец `mark_disconnected`** ([keeper/reaper.md](../keeper/reaper.md)) — **ленивое согласование снимка В ОБЕ СТОРОНЫ**: метит `connected` → `disconnected` по stale `last_seen_at` при мёртвом SID-lease (lease-aware, idle-Soul на живом lease не трогает) **и** `disconnected` → `connected` при живом SID-lease (Soul реально online — реконнект захватил lease, а PG-снимок остался `disconnected`). Это приведение PG-снимка к факту фоном, а не источник presence — прогон от снимка не зависит. Обычный reconnect снимок **напрямую не двигает** (eventstream presence в PG на hot-path не пишет); его двигает именно Жнец-reconcile по факту живого lease. Без обратного направления снимок латчился бы в `disconnected` навсегда после первого «обрыв+sweep».

`last_seen_at` — отдельный snapshot «когда последний раз видели» (throttled-flush из стрима в PG, real-time-значение в Redis-heartbeat), нужный Operator API и Жнецу; он тоже **не** presence-предикат.

Соответствующий cascade в той же PG-транзакции:

- `souls.status` → `destroyed`;
- активные `soul_seeds` SID → `orphaned` (`active` → `orphaned`; `revoked` НЕ перетирается — precedence revoked > orphaned);
- ещё-не-использованные `bootstrap_tokens` SID → `used_at = NOW()`, `used_by_kid = 'system-cloud-destroy'` (anti-replay-инвариант: токен «погашен в момент cloud-destroy», даже если он не был предъявлен).

Диаграмма — переходы **lifecycle-снимка `souls.status`** (для Operator API),
НЕ presence: online/offline решает Redis SID-lease отдельно (см. выше).

```
   операторский запрос          первое успешное подключение
   SoulSeed-токена              (handshake + match fingerprint)
       │                                  │
       ▼                                  ▼
   ┌─────────┐  TTL 24h        ┌─────────────────┐  Жнец: stale     ┌──────────────┐
   │ pending │ ─── Жнец ──►    │   connected     │  + нет lease     │ disconnected │
   └─────────┘   expired       └─────────────────┘ ──────────────► └──────┬───────┘
        │                              ▲           (lazy reconcile →)      │
        │                Жнец reconcile (живой lease) / Bootstrap-RPC      │
        │                              └──────────────────────────────────┘
        │                                  (← lazy reconcile)
        │  TTL 24h без                                    Жнец, max_age 30d
        │  использования                                  (если так и не
        ▼                                                  вернулся)
   запись удаляется                                        запись удаляется
   Жнецом                                                  Жнецом
                                                                │
                            оператор отозвал ──────► ┌─────────┐
                                                     │ revoked │
                                                     └─────────┘
                                                                │
                cloud-destroy (ADR-017) ───────────► ┌───────────┐
                                                     │ destroyed │ (terminal,
                                                     └───────────┘  не в default purge_souls)
```

## On-disk-формат `paths.seed` (нормативно)

`paths.seed` — **каталог** (mode `0700`) с версионной раскладкой. Активная версия выбирается через симлинк `current`; ротация переключает его атомарно, чтобы на диске никогда не было рассинхронизированной пары `cert↔key`.

```
paths.seed/
  current -> v3          # ОТНОСИТЕЛЬНЫЙ симлинк на активную версию
  v2/                    # предыдущая версия (хранится для отката-страховки)
    cert.pem  key.pem  ca.pem
  v3/                    # активная версия
    cert.pem  (0644)
    key.pem   (0400)
    ca.pem    (0644)
```

- **Каталоги версий** — `vN/` (mode `0700`), нумерация монотонно возрастающая (`v1`, `v2`, …); новая версия = `max(существующие) + 1`.
- **Файлы версии** — фиксированные имена `cert.pem` (`0644`) / `key.pem` (`0400`) / `ca.pem` (`0644`).
- **`current`** — относительный симлинк (`current -> v3`, не абсолютный путь), прозрачный для open(2): tls-конфиг читает материал через `current/{cert,key,ca}.pem`, поэтому swap версии меняет источник без переинициализации путей.
- **Hard-cut (M1):** старый плоский формат (`cert.pem`/`key.pem`/`ca.pem` прямо в `paths.seed` без `current`) **не поддерживается**. Авто-миграции нет: если в каталоге лежит плоский формат без `current` — Soul считает bootstrap не выполненным (оператор делает `soul init` заново).

### Запись новой версии и атомарный swap

`soul init` (первичный bootstrap) и ротация используют один путь записи:

1. **Валидация пары `cert↔key`** через X509 — fail-fast **до** любой записи на диск. Несогласованную пару на диск не пишем; текст ошибки не содержит приватный ключ.
2. **Запись всей версии** в `vN+1/`: три файла атомарно (temp + chmod + fsync + rename), затем **fsync каталога версии** — без него rename-ы файлов могут не дойти до диска до сбоя, и версия окажется неполной.
3. **Атомарный swap**: temp-симлинк `-> vN+1` рядом, `rename(2)` поверх `current` (атомарно на POSIX), затем **fsync каталога `paths.seed`**.
4. **Очистка** (best-effort, после успешного swap): хранятся активная версия и одна предыдущая; версии старше удаляются. Ошибка очистки не фейлит запись.

До шага 3 `current` указывает на прежнюю версию — **сбой на шагах 1–2 оставляет валидную прежнюю активную версию** (crash-safety): новая версия пишется рядом и становится активной одним атомарным переключением симлинка.

### Чтение

Чтение идёт через `current/{cert,key,ca}.pem`. После чтения пара `cert↔key` проверяется на согласованность через X509:

- нет `current` (или в активной версии отсутствует один из трёх файлов) → `ErrIncomplete` («bootstrap не выполнен», подсказка `run soul init`);
- пара `cert↔key` рассинхронизирована (например, версия частично перетёрта мимо атомарного swap-а) → `ErrMismatched` (отличается от `ErrIncomplete`: «материал есть, но не образует валидную пару»).

## Ротация SoulSeed

- Период по умолчанию — **раз в неделю**.
- Soul по живому стриму просит у Keeper новый SoulSeed заранее (за `expires_at - 24h`).
- Keeper выпускает новый, возвращает Soul-у. Soul разворачивает на хосте по схеме [On-disk-формат → запись и swap](#запись-новой-версии-и-атомарный-swap) (новая версия `vN+1/` рядом, затем атомарный swap симлинка `current`), переключается на новый сертификат.
- В `soul_seeds` создаётся новая строка `active`; предыдущая помечается `superseded`.
- Жнец позже подбирает старые `superseded` / `expired` записи.

Ротация происходит исключительно по живому стриму; никакого отдельного re-bootstrap-флоу через токен не требуется. Если стрим оборвался и Soul долго не приходил — после возвращения он сначала подключается на старом seed-е, затем инициирует ротацию.

## Отзыв (`revoke`)

- Операция оператора через API/MCP. Изменяет `souls.status = 'revoked'` и `soul_seeds.status = 'revoked'` для всех активных/живых seed-ов SID.
- На TLS-уровне Keeper при следующем handshake отказывает в подключении (через CRL или прямой матч fingerprint в БД с фильтром по статусу).

## Переименование хоста

In-place rename SID не поддерживается. Если у хоста изменился FQDN — это **новая Soul**: оператор создаёт новый SoulSeed-токен под новый SID и устанавливает Soul на хост заново; старая запись `revoked` или подбирается Жнецом. Решение принято осознанно: SID = FQDN, миграция SID = новая инсталляция.

## См. также

- [onboarding.md](onboarding.md) — жизненный цикл bootstrap-токена, SQL-транзакция предъявления, доставка.
- [connection.md](connection.md) — как Soul с уже выпущенным SoulSeed-ом подключается к Keeper-у.
- [config.md](config.md) — где на хосте лежат `paths.seed` и `tls.ca`.
- [architecture.md → Жизненный цикл Soul и реестр душ](../architecture.md#жизненный-цикл-soul-и-реестр-душ) — архитектурный обзор.
- [architecture.md → Reaper / Жнец](../architecture.md#reaper--жнец) — правила GC реестров.
- [naming-rules.md](../naming-rules.md) — словарь имён (SID, KID, SoulSeed, Coven).
