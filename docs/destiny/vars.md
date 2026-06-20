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

Допустимые value-типы: те же, что и в `input:` ([docs/input.md](../input.md)) — string / integer / number / boolean / array / object. Шаблонные выражения `"${ … }"` поддерживаются на любой глубине ([ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [docs/templating.md](../templating.md)).

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

Не доступно (намеренно):

- **`vars.<other>`** — другие переменные `vars.yml`. Это запрет перекрёстных ссылок между vars: они вычисляются параллельно, а не в порядке появления. Если нужна цепочка — собирай выражение целиком.
- **`register.<name>`** — результаты задач. На момент вычисления `vars` задач ещё не было.
- **`essence.*`** — этого пространства имён в destiny **нет вообще**. essence — концепция уровня service; service сам решает, какие значения подкладывать в `input:` destiny при вызове.

## Слияние file-vars ↔ task-vars (Вариант A)

Пространство имён `vars.*` делят два источника: file-level `vars.yml` (этот документ) и task-level `vars:` на отдельной задаче ([tasks.md §9](tasks.md)). Когда имя объявлено в обоих — действует **Вариант A**:

- **task-level `vars:` переопределяет одноимённый file-level var.** File-vars — базовый слой, task-vars кладутся поверх. Исход детерминирован: на задаче с собственным `vars: { redis_unit_name: … }` именно task-значение попадёт в `${ vars.redis_unit_name }`, а file-level — нет.
- **Оба слоя резолвятся над одним базовым контекстом (`input.*` + `soulprint.self.*` + `incarnation.*`) и НЕ видят друг друга.** file-var не может сослаться на task-var, task-var — на file-var, ни тот ни другой — на свой же слой (`vars.<other>` внутри значения даёт no-such-key). Это та же изоляция, что запрет перекрёстных ссылок внутри `vars.yml` (см. «Что доступно внутри `vars`»): порядок объявления безразличен.
- **Изоляция scope сохраняется.** file-vars резолвятся внутри destiny-прохода (после валидации `apply.input`), `register.*`/`essence.*`/`soulprint.hosts` им недоступны — как и task-vars destiny-задачи. scenario-level `vars:` в destiny НЕ видны вовсе (только через `apply: input:`).
- **`soul-lint` поднимает `warn` (`vars_collision`)** на каждое имя, объявленное и в `vars.yml`, и в task-level `vars:` той же destiny. Это не ошибка (Вариант A однозначен), но почти всегда — недосмотр автора: переименуй один из двух или полагайся на переопределение осознанно.

Резолв file-vars выполняется **один раз на destiny-проход** (per-host, потому что значения могут ссылаться на `soulprint.self`), а не на каждую задачу: file-vars инвариантны по задачам одного прохода.

## См. также

- [manifest.md](manifest.md) — раскладка папки destiny, где лежит `vars.yml`.
- [input.md](input.md) — внешний контракт destiny.
- [tasks.md](tasks.md) — template-контекст задач, где `vars.*` доступны.
