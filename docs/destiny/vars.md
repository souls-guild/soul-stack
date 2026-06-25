# `vars.yml` — destiny-локалы

Файл `vars.yml` рядом с `destiny.yml` объявляет **локальные переменные destiny** — статичные значения, которые автор destiny прибил гвоздями и которые недоступны для переопределения снаружи. В задачах доступны как `${ vars.<name> }` (в строковой интерполяции; в top-level expression-keys типа `when:` — голая `vars.<name>`, см. [`docs/templating.md`](../templating.md)).

## Зачем

Без `vars:` у destiny есть две крайности:

- **Захардкодить** пути/имена/префиксы прямо в задачи (`params: { path: "/etc/redis/redis.conf", ... }`) — копипаст по всем задачам, ад при изменении.
- **Прокидывать** их через `input:` — но тогда они входят во **внешний контракт** и кто-то снаружи может (а значит, обязательно сделает) их переопределить. А автор имел в виду, что это инвариант destiny, а не точка вариативности.

`vars:` — третий путь: переменные есть, использовать удобно, но снаружи их не видно и не передёрнуть.

## Семантика

- **Source of truth — `destiny-<name>/vars.yml`** (рядом с `destiny.yml`).
- **Изолированы в destiny.** Ни scenario, ни оператор через API, ни essence service-а **не** перебивают значения. Если оператору нужна возможность подмены — соответствующее значение должно быть в `input:`, не в `vars:`.
- **Могут ссылаться на `input.*`.** Vars вычисляются **после** валидации input — то есть выражение `"/etc/redis/users/${ input.user }.acl"` валидно. Обратное (input ссылается на vars) — нет: input приходит снаружи, до того, как vars вообще существуют.
- **Доступны во всех задачах** одного destiny — в `tasks/main.yml` и в любом подключаемом через `include:` соседе.

## Формат файла

Top-level YAML-map. Никакой обёртки (`vars:` ключом верхнего уровня внутри файла — тавтологично, путь к файлу и так сообщает контекст).

```yaml
# redis/vars.yml
redis_unit_name: redis-server
redis_conf_path: /etc/redis/redis.conf
redis_data_dir:  /var/lib/redis
redis_user:      redis
redis_group:     redis

# Допускается ссылка на input.*
acl_file_path: "/etc/redis/users/${ input.user }.acl"
```

Допустимые value-типы: те же, что и в `input:` ([docs/input.md](../input.md)) — string / integer / number / boolean / array / object. Шаблонные выражения `"${ … }"` ([ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [docs/templating.md](../templating.md)) резолвятся **только когда всё значение var — строка** (верхний уровень значения). non-string значения (map / list / число / bool) проходят **литералом насквозь** — CEL их не трогает.

> **Ограничение (известное): `${ … }` внутри map/list-значений НЕ разворачивается.** Если значение var — map или list, вложенные `${ … }` в его элементах остаются **сырым текстом**, а не резолвятся. Резолв применяется к строковому значению целиком, не рекурсивно по структуре.
>
> ```yaml
> base: /etc/redis                       # string — ок
> conf_path: "${ vars.base }/redis.conf" # string — резолвится в "/etc/redis/redis.conf"
>
> paths:                                  # map-значение — НЕ резолвится
>   conf: "${ vars.base }/redis.conf"     # останется литералом "${ vars.base }/redis.conf"
> ```
>
> Если нужна сборка вложенной структуры из других vars — собирай каждый строковый лист отдельным var, либо строй map целиком одним `${ … }`-выражением (вся ячейка = один маркер → нативный тип, см. [templating.md](../templating.md)). Рекурсивный рендер по глубине map/list — не реализован.

## Что НЕ лежит в `vars.yml`

- **Переопределяемые снаружи параметры.** Если оператор должен иметь возможность подменить значение — это `input:`-параметр, а не `vars`-локал. Граница между ними — это граница «контракт vs внутренности».
- **Секреты.** Vault-ссылки и пароли приходят через `input:` с `secret: true` (см. [input.md](input.md) → раздел про `secret:`). В `vars.yml` секретов быть не должно — он закоммичен в git destiny-репо.
- **Значения, специфичные для конкретного incarnation.** Имена кластеров, FQDN мастеров, capacity-цифры — это `input:`, потому что они **меняются** между incarnation-ами одного и того же сервиса. `vars:` — про инварианты, одинаковые для всех incarnation.

## Использование в задачах

```yaml
# redis/tasks/apply.yml
- name: Install redis-server package
  module: core.pkg.installed
  params:
    name: "${ vars.redis_unit_name }"   # "redis-server"
    version: "${ input.version }"       # параметр от caller

- name: Render redis.conf from template
  module: core.file.rendered             # ADR-010: рендер делает core.file.rendered, не core.file.present + render()
  params:
    path: "${ vars.redis_conf_path }"
    template: templates/redis.conf.tmpl
    vars:
      maxmemory: "${ input.maxmemory }"
    mode: "0640"
    owner: "${ vars.redis_user }"
    group: "${ vars.redis_group }"
```

## `vars` vs `input` — таблица отличий

| | `input.<name>` | `vars.<name>` |
|---|---|---|
| **Источник** | caller (scenario.apply.input или прямой API-вызов) | `destiny-<name>/vars.yml` |
| **Кто решает значение** | оператор / service / тест | автор destiny |
| **Описано в схеме?** | да, `input:` в `destiny.yml` ([input.md](input.md)) | нет, plain map |
| **Валидируется?** | да, два раунда (Keeper + Soul) | нет (это значения от самого destiny-разработчика) |
| **Переопределяется снаружи?** | да — это и есть его смысл | **нет** |
| **Видно в логах apply?** | да (значения параметров видны как часть аудита) | да |
| **Маскируется (`secret`)?** | да, через `secret: true` в схеме | нет — секреты сюда не пишутся |
| **Видно в API-ответе** | как `input:` блок | как `vars:` блок |

## Что доступно внутри `vars` через шаблоны

В выражениях `"${ … }"` (CEL-интерполяция, см. [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)) на правой стороне `vars.yml` доступно:

- `input.<name>` — провалидированные параметры destiny.
- `soulprint.self.<name>` — факты текущего хоста ([ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp): `soulprint.self.os.family`, `soulprint.self.network.primary_ip`, `soulprint.self.memory.total_mb`, …).
- **`vars.<other>`** — другая переменная `vars.yml` ТОГО ЖЕ слоя (см. ниже «var → var»).

Не доступно (намеренно):

- **`register.<name>`** — результаты задач. На момент вычисления `vars` задач ещё не было.
- **`essence.*`** — этого пространства имён в destiny **нет вообще**. essence — концепция уровня service; service сам решает, какие значения подкладывать в `input:` destiny при вызове.
- **`soulprint.hosts` / `soulprint.where(...)`** — cross-host scenario-only аксессоры. В destiny-проходе отсекаются изоляцией (ошибка на compile). var → var их НЕ открывает.

### var → var (ссылки внутри слоя)

`vars.yml`-переменная **может** ссылаться на другую переменную ТОГО ЖЕ `vars.yml` через `${ vars.<other> }` (ADR-009 / ADR-010 amendment 2026-06-24). Резолв **eager-topological**:

- Зависимости извлекаются из CEL-AST (не regex): `${ vars.X }` в значении → ребро на `X`.
- Слой резолвится в **топологическом порядке** — переменная видит уже вычисленные зависимости. **Порядок объявления в файле безразличен**:

  ```yaml
  # эквивалентно при любом порядке строк
  root_owner: root
  root_group: "${ vars.root_owner }"   # резолвится в "root"
  ```

- **Цикл** (`a → b → c → a`, в т.ч. самоссылка `a → a`) → ошибка рендера `var_cycle` с трассой цикла.
- **Ссылка на несуществующий ключ слоя** (`vars.z`, которого нет) → ошибка рендера `var_unknown_ref`. Проверка **eager**: ошибка поднимается, даже если сам ссылающийся var нигде не используется (битая ссылка = опечатка автора, не «отложенный» var).
- **Только внутри своего слоя.** var→var НЕ ослабляет изоляцию: ссылка по-прежнему не достаёт `register.*`/`essence.*`/`soulprint.hosts`. Цепочка между file-слоем и task-слоем недоступна (см. ниже).
- Index-форма `vars['key']` не поддерживается — используй select-форму `vars.key` (имя ключа должно быть статически известно из AST).

## Слияние file-vars ↔ task-vars (Вариант A)

Пространство имён `vars.*` делят два источника: file-level `vars.yml` (этот документ) и task-level `vars:` на отдельной задаче ([tasks.md §9](tasks.md)). Когда имя объявлено в обоих — действует **Вариант A**:

- **task-level `vars:` переопределяет одноимённый file-level var.** File-vars — базовый слой, task-vars кладутся поверх. Исход детерминирован: на задаче с собственным `vars: { redis_unit_name: … }` именно task-значение попадёт в `${ vars.redis_unit_name }`, а file-level — нет.
- **var → var работает ВНУТРИ каждого слоя, но НЕ между слоями.** file-var может ссылаться на другой **file-var** (eager-topological, см. «var → var» выше); task-var — на другой **task-var** того же слоя. А вот **межслойно** ссылки запрещены: file-var не видит task-var, task-var не видит file-var (`${ vars.<чужой_слой> }` даёт `var_unknown_ref`). task-vars резолвятся над тем же базовым контекстом (`input.*` + `soulprint.self.*` + `incarnation.*`), что и file-vars, и file-слой подкладывается под них только ПОСЛЕ резолва (override) — поэтому task-var не может сослаться на file-var.
- **Изоляция scope сохраняется.** file-vars резолвятся внутри destiny-прохода (после валидации `apply.input`), `register.*`/`essence.*`/`soulprint.hosts` им недоступны — как и task-vars destiny-задачи. var→var изоляцию НЕ ослабляет. scenario-level `vars:` в destiny НЕ видны вовсе (только через `apply: input:`).
- **`soul-lint` поднимает `warn` (`vars_collision`)** на каждое имя, объявленное и в `vars.yml`, и в task-level `vars:` той же destiny. Это не ошибка (Вариант A однозначен), но почти всегда — недосмотр автора: переименуй один из двух или полагайся на переопределение осознанно.

Резолв file-vars выполняется **один раз на destiny-проход** (per-host, потому что значения могут ссылаться на `soulprint.self`), а не на каждую задачу: file-vars инвариантны по задачам одного прохода.

## См. также

- [manifest.md](manifest.md) — раскладка папки destiny, где лежит `vars.yml`.
- [input.md](input.md) — внешний контракт destiny.
- [tasks.md](tasks.md) — template-контекст задач, где `vars.*` доступны.
