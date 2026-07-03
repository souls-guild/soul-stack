# ADR-067. Обязательный log-shipping (Vector) — лог-плоскость data-сервисов

> **Статус: active, реализовано.** Дизайн architect-а (2026-07-01, `.pm/tasks/2026-07-01-vector/`), решения пользователя (sink-развилка A / имя `vector`). Фиксация **ретроспективная** (2026-07-03, погашение drift «реализация есть — ADR нет»): destiny `vector` и встраивание в redis/dragonfly уже в дереве.
>
> **Примечание к нумерации.** Дизайн 2026-07-01 планировал номер «ADR-065»; за это время 0064 занял secret-write-path, 0065 — `core.module.installed`, 0066 — teleport-onboarding. Поэтому ADR — **0067**. Ссылки `ADR-065` в `examples/**` (essence/create/covenant/migration) на этот слайс — тот же исторический сдвиг, чинятся отдельно.

**Контекст.** У data-сервисов (redis, dragonfly, далее mongo/qdrant) уже есть **метрики**: `node-exporter` (метрики хоста) и `redis_exporter` (метрики Redis) — Prometheus-pull, [ADR-024](0024-observability.md). Метрики отвечают на «сервис жив, нагрузка такая», но не на «что именно произошло» — для инцидент-разбора нужны **логи** демона, централизованно и в реальном времени, а не `ssh + tail` по хостам. Пул (метрики) и пуш (логи) — разные плоскости: exporter ждёт scrape, лог-агент сам толкает строки в коллектор. Лог-плоскости у сервисов не было; каждый оператор решал сбор логов сам. Требование пользователя: log-shipping ставится **во все базовые data-сервисы обязательно**, «как node-exporter» — инвариантом сервиса, а не опцией.

## Решение

**(a) Vector как лог-плоскость, дополняющая экспортеры.** Ставим [Vector](https://github.com/vectordotdev/vector) (`vectordotdev/vector`) — observability-агент `sources → transforms → sinks`, PUSH логов. Vector **дополняет**, не заменяет node-exporter/redis_exporter: три независимых слоя наблюдаемости (метрики хоста pull / метрики Redis pull / логи push), не пересекаются. Имя `vector` — upstream-продукта (прецедент `node-exporter`/`redis-exporter`; управляемый инструмент, не сущность Soul Stack), зафиксировано в [naming-rules](../naming-rules.md).

**(b) Дизайн — клон эталона node-exporter (stateful-ветка).** Новая standalone-destiny [`examples/destiny/vector/`](../../examples/destiny/vector/destiny.yml) повторяет прод-конвенцию [`production-conventions.md`](../destiny/production-conventions.md) (эталон [`examples/destiny/node-exporter/`](../../examples/destiny/node-exporter/), stateful-ветка §2):

- **install** — `core.url.fetched` (release-tarball, https-only, `allow_private`-opt-out под internal-mirror) с **обязательным** `sha256` (fail-closed: верификация ДО материализации файла) → `core.archive.extracted`;
- **account** — `core.group.present` + `core.user.present` (`system`, `/usr/sbin/nologin`, без home) под **стабильным uid** (НЕ `DynamicUser`): у Vector persistent disk-buffer (`data_dir`) — очередь сдвигов прочитанных файлов и sink-а переживает рестарт и требует фиксированного владельца; `core.file.directory` для `data_dir` (0700) и `config_dir` (0750);
- **service** — `core.file.present src:` (бинарь) + `core.file.rendered` `vector.yaml` (sources/sinks) + `core.file.rendered` hardened systemd-unit + `core.service.running` (enabled) + `core.service.restarted` `onchanges:[config, unit, binary]`;
- **arch** — Vector публикует релизы с Rust-триплетом (`vector-<v>-<arch>-unknown-linux-gnu.tar.gz`), поэтому `soulprint.self.os.arch` (`amd64`/`arm64`) маппится в `x86_64`/`aarch64` CEL-ом в `vars` (отличие от Prometheus-экспортеров, где arch = `amd64` напрямую).

Destiny собрана **целиком из существующих core-модулей** ([ADR-015](0015-core-modules-mvp.md)) — **нового core-модуля не вводит**. Рендер `vector.yaml`/unit — `core.file.rendered` (text/template, [ADR-010](0010-templating.md)).

**(c) Встраивание — безусловный `apply: destiny vector` в конце `create`.** После deploy-ветки и экспортеров `create` **безусловно** (без `when:`-гейта) разворачивает vector на каждый хост инкарнации композицией переиспользуемой destiny через `apply: destiny` (изолированный render, [ADR-009](0009-scenario-dsl.md)) — [`redis/scenario/create/main.yml`](../../examples/service/redis/scenario/create/main.yml). Это **инвариант data-сервиса**, а не операторский выбор. Весь контракт — **в essence** (контракт A — author-context: версии/checksum/sink скрыты от Run-формы, оператор переопределяет в `spec.essence`); `covenant`/`form` не трогаются. Per-сервис essence несёт блок `vector_*`; единственное отличие сервисов — `vector_log_sources` ([redis](../../examples/service/redis/essence/_default.yaml): `/var/log/redis/*.log`; [dragonfly](../../examples/service/dragonfly/essence/_default.yaml): `/var/log/dragonfly/*.log` + `/var/log/redis/*.log` для sentinel-демона).

**(d) sink — Вариант A (essence per-incarnation).** Адрес центрального коллектора живёт в essence: `vector_sink_type` (`loki`/`elasticsearch`/`vector`/`console`) / `vector_sink_endpoint` / `vector_sink_auth_ref`. Дефолт `sink_type: console` — логи в собственный stdout Vector, **без внешней инфры** (безопасный пилотный дефолт; коллектор не обязателен). `loki`/`elasticsearch`/`vector` требуют `sink_endpoint`. **`sink_auth_ref` в state НЕ оседает** — секрет (Vault-ref ИЛИ уже зарезолвленное значение, каскад: caller передаёт ref, резолв Soul-side; симметрия `tls.*_ref`), пробрасывается в unit через `Environment=VECTOR_SINK_TOKEN`, а не в `vector.yaml` на диске. Следует установленной vault-ref-конвенции (Herald `secret_ref` [ADR-052](0052-herald-notifications.md) / Provider `credentials_ref` [ADR-017](0017-keeper-side-core.md) / Augur `auth_ref` [ADR-025](0025-augur.md)) — это read/resolve-путь, не write-path.

**(e) state read-model.** state дополняется объектом `logging.vector_*` (`version`/`sink_type`/`sink_endpoint`/`log_sources` — **без** `auth_ref`, секрет исключён) — [`redis/covenant.yml`](../../examples/service/redis/covenant.yml), симметрия объекту `monitoring` экспортеров. Bump `state_schema` + миграция **per-сервис** ([redis `013_to_014.yml`](../../examples/service/redis/migrations/013_to_014.yml): v13→v14, forward-only, `has()`-guard идемпотентен; консервативный дефолт для до-vector-инкарнаций: `version: ''` = «не развёрнуто», `console`). dragonfly — аналогичной миграцией.

## Альтернативы (sink)

- **A — essence per-incarnation (выбрано, пилот).** Нулевая инфра, дефолт `console` работает из коробки. Минус: дубль `sink_endpoint` по инкарнациям.
- **B — `keeper.yml` глобально (follow-up).** Один адрес коллектора на кластер. Минус: новый config-контракт `keeper.yml` → scenario-CEL (well-known keeper-settings) — заводить, когда появится общий коллектор.
- **C — гибрид (follow-up).** keeper-default + essence-override. Комбинирует A и B; вводится после B.

## Consequences

- Новая standalone-destiny `vector` + блок `vector_*` в essence каждого data-сервиса + объект `logging` в state (+ миграция per-сервис). Тиражирование на новый сервис — копия блока essence + миграция + шаг `apply:` (как экспортеры).
- **Нового core-модуля / нового имени словаря нет**: destiny из существующих core-модулей, имя `vector` уже в [naming-rules](../naming-rules.md).
- Оператор без коллектора получает валидный конфиг (`console`) — vector стартует, читает логи, не требует внешней инфры; при живом Grafana Loki/ES оператор задаёт `sink_type`/`endpoint`/`auth_ref` в `spec.essence`.
- Секрет доступа к коллектору не попадает ни в `vector.yaml` на диске, ни в `incarnation.state` — только в systemd `Environment` и Vault.
- **Открыто (follow-up):** централизованный `sink_endpoint` (Вариант B/C); тираж на mongo (эпик mongo); qdrant — **отдельный эпик** (векторная БД, не log-shipping).

## Amends / Related

- **Amends [ADR-024](0024-observability.md) (Observability).** Добавляет **лог-плоскость (push)** третьим измерением наблюдаемости рядом с метриками (Prometheus pull) и трейсами (OTel-bridge). ADR-024 логи не покрывал.
- **Related — эталон node-exporter (НЕ amend):** структурный клон [`production-conventions.md`](../destiny/production-conventions.md) (stateful-ветка) / [`examples/destiny/node-exporter/`](../../examples/destiny/node-exporter/). У node-exporter **своего ADR нет** — amend'ить нечего, это связь «клон паттерна» (корректирует фантомный «ADR-064 node-exporter» из дизайна 2026-07-01: реальный [ADR-064](0064-secret-write-path.md) — secret-write-path, к vector не относится).
- **Related — [ADR-015](0015-core-modules-mvp.md) (core-модули):** destiny собрана из существующих core-модулей, нового модуля не вводит (связь «использует», не amend).
- **Related — [ADR-010](0010-templating.md)** (`core.file.rendered` для `vector.yaml`/unit), **[ADR-009](0009-scenario-dsl.md)** (`apply: destiny` — изолированная композиция), **[ADR-007](0007-versioning-git-ref.md)** (версия destiny = git ref, поля `version:` нет).
