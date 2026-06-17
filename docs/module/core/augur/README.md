# core.augur

Read-probe **живого** доступа к внешней системе (Vault / Prometheus / ELK) через
брокер [Augur](../../../keeper/augur.md) ([ADR-025](../../../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul)).
**Soul-side**, статически встроен в `soul`-бинарь. Реализация —
[`soul/internal/coremod/augur/augur.go`](../../../../soul/internal/coremod/augur/augur.go).

В отличие от pre-resolved-модели (всё внешнее резолвится Keeper-ом **до**
отправки `ApplyRequest`), `core.augur.fetch` запрашивает значение **в момент
apply на хосте** — для случаев, которые pre-resolve не может отдать заранее:
короткоживущий dynamic-secret, live-метрика как условие шага, чтение из ELK в
ходе прогона. Граница [ADR-012(d)](../../../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)
не двигается: в MVP-1 (`delegate=false`) данные приходят inline **через Keeper**,
внешний credential на Soul не попадает.

Это verb-модуль: единственное состояние — `fetch` (без declarative-семантики
«привести к состоянию»). Типичное использование `register:` — read-only probe
(`changed_when: false` не нужен — модуль уже `changed=false`) с чтением
`register.<name>.*` в последующих `where:` / `failed_when:` / `output:`.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `fetch` | Запросить значение из Omen-а у брокера Augur в момент apply. | `changed=false` **ВСЕГДА**, конструктивно и ненастраиваемо: read-probe не меняет состояние хоста (прецедент — [`core.http.probe`](../http/README.md), [`core.exec.run`](../exec/README.md)). |

## fetch — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `omen` | string | required | Имя Omen-а (`omens.name`) — внешней системы, к которой запрашивается доступ. |
| `query` | string | required | Запрос к Omen-у: KV-путь (vault, в т.ч. `#field`-проекция), promQL (prometheus), index-query (elk). Сверяется против `Rite.allow` на Keeper-е. |

`request_id` и `apply_id` модуль выставляет сам (не params): `request_id`
генерируется Soul-ом (ULID, уникален per-stream, [§5.1 augur.md](../../../keeper/augur.md#51-augurrequest-soul--keeper)),
`apply_id` берётся из контекста прогона.

## Output / register

При `status=ok` модуль кладёт `AugurReply.inline_data`
([`google.protobuf.Struct`](../../../keeper/augur.md#53-форма-inline_data-shape-convention))
в register **как есть**:

- **скаляр** (например vault-KV `#field`) — объект `{ "value": <scalar> }`
  (Keeper заворачивает скаляр в ключ `value` — `Struct` не несёт голый скаляр);
- **map** (vault KV целиком / Prometheus-результат / ELK-ответ) — натуральный
  объект (ключи исходной map-ы становятся ключами register).

Проекцию `#field` делает **Keeper** при чтении Omen-а — Soul получает уже
спроецированное значение и не видит секрет целиком.

## Контракт ошибок

Шаг падает (`failed`), значение не отдаётся, при любом из:

- **`denied`** — авторизация Keeper-а отклонила (Omen не найден / Soul не в Rite
  / query вне allow-list, [§6 augur.md](../../../keeper/augur.md#6-авторизация-keeper-side));
- **`error`** — сбой исполнения на Keeper-е / Omen (внешняя система недоступна);
- **`unspecified`** (default-deny) — отсутствие явного `ok` трактуется как
  **запрет** ([§5.1 augur.md](../../../keeper/augur.md#51-augurrequest-soul--keeper)),
  защита от рассогласования;
- брокер недоступен в прогоне (push-режим `soul apply` без EventStream-сессии);
- таймаут / разрыв стрима до ответа (отмена ctx / закрытие сессии).

Сообщение об ошибке несёт имя Omen-а для диагностики, но **не** `query` и **не**
само значение (query может нести путь к секрету; значения/токены в диагностику
не пишутся — [§8 augur.md](../../../keeper/augur.md#8-audit)).

## Пример

```yaml
# Live-чтение dynamic-secret в момент apply (delegate=false: данные через Keeper).
- name: Fetch DB credentials at apply time
  module: core.augur.fetch
  register: db_creds
  params:
    omen: vault-prod
    query: database/creds/app-role#password

# Использование значения в последующем шаге.
- name: Write app config
  module: core.file.rendered
  params:
    path: /etc/app/config.yml
    template: app-config.tmpl
    vars:
      db_password: ${ register.db_creds.value }
```

## Безопасность

- **Master-credential на Soul НИКОГДА не попадает** (нормативный инвариант
  Augur, [§безопасности augur.md](../../../keeper/augur.md#требование-безопасности-нормативный-инвариант)).
  В MVP-1 (`delegate=false`) Soul вообще не получает credential — только значение
  inline через Keeper. Делегация (`delegate=true`, scoped-токен / static-cred) —
  MVP-2, этим модулем **не** обрабатывается (на OK без `inline_data` модуль даёт
  ошибку шага).
- **Авторизация — на Keeper-е, не на Soul.** Soul не решает, что ему можно: он
  шлёт запрос, Keeper сверяет SID (из mTLS peer cert, не из payload) → covens →
  Rite → allow-list. Любая непройденная проверка → `denied` без значения.
- **Default-deny.** `unspecified`-статус трактуется как запрет — отсутствие
  явного `ok` никогда не «продолжает».
- **Минимизация секрета в диагностике.** В сообщения об ошибке не уходят ни
  `query`, ни значение, ни токен.

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [keeper/augur.md](../../../keeper/augur.md) — спека брокера Augur (транспорт §5, авторизация §6, inline_data §5.3, audit §8).
- [ADR-025](../../../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul) — фиксация дизайна Augur.
- [core/http/README.md](../http/README.md) / [core/exec/README.md](../exec/README.md) — прецеденты read-probe (`changed=false` конструктивно).
- [naming-rules.md → Augur](../../../naming-rules.md#augur-вложенные-proto-типы-и-реестры) — словарь имён (Omen / Rite / AugurStatus).
