# core.git

Клонирование и обновление git-репозитория на хосте. **Soul-side**, статически
встроен в `soul`-бинарь. Реализация — [`soul/internal/coremod/git/git.go`](../../../../soul/internal/coremod/git/git.go).

Вызывает системный `git` как подпроцесс (clone / pull / rev-parse); собственного
git-клиента модуль не содержит. MVP сознательно **не** покрывает смену remote URL,
submodule, lfs и sparse-checkout — слишком много развилок для первой версии.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `cloned` | По пути `path` лежит git-репо. | `changed=true`, если `path/.git` отсутствовал и репозиторий был склонирован. Если `path/.git` уже есть — `changed=false` (содержимое не трогается, новый pull не выполняется). |
| `pulled` | По пути `path` лежит git-репо, подтянутый до remote (`git pull --ff-only`). | Если `path/.git` отсутствует — clone, `changed=true`. Если репо есть — `git pull --ff-only`; `changed=true` только когда `HEAD` сдвинулся (сверка `rev-parse HEAD` до и после). |

## cloned — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `repo` | string | required | URL/адрес репозитория. Передаётся в `git clone` после `--` (репо, начинающийся с `-`, не распарсится как опция — argument-injection guard, security). |
| `path` | string | required | Целевой каталог клона. Наличие `path/.git` — критерий идемпотентности. |
| `branch` | string | optional (default `main`) | Ветка для `--branch`. Если не задан — `main`. |
| `depth` | int | optional | Глубина shallow-клона (`--depth`). Применяется только при `depth > 0`; не задан → полный клон. |

## pulled — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `repo` | string | required | URL/адрес репозитория. Используется при clone-if-missing (тот же `--`-guard). |
| `path` | string | required | Каталог репо. Если `path/.git` отсутствует — сначала clone, затем семантика «обновлено». |
| `branch` | string | optional (default `main`) | Ветка для clone-if-missing. На сам `git pull --ff-only` не передаётся. |
| `depth` | int | optional | Глубина shallow-клона при clone-if-missing (`--depth`, только при `depth > 0`). |

## Capabilities / side-effects

- **Выполняет подпроцессы:** `git clone` / `git pull --ff-only` / `git rev-parse HEAD`.
- **Меняет файловую систему:** создаёт/обновляет каталог `path`. Для системных
  путей требует соответствующих прав.
- **Сетевой доступ:** clone/pull ходят на remote `repo`. Транспорт и аутентификация —
  на стороне системного `git` (ssh-agent, credential helper, `~/.netrc` и т.п.);
  модуль их не настраивает.
- **`pull` — только fast-forward** (`--ff-only`): расходящаяся локальная история не
  мёржится силой, шаг падает (защита от потери локальных коммитов на хосте).

## Output / register

`cloned`/`pulled` отдают `{ path, cloned: true, head }`, где `head` — текущий
`HEAD` (sha из `git rev-parse HEAD`). `head` — best-effort: если `rev-parse` не
отдал sha, поле пустое (на основной flow это не влияет).

## Пример

`cloned` — выложить репозиторий на хост (минимальный пример):

```yaml
- name: Clone deploy repo
  module: core.git.cloned
  params:
    repo: https://github.com/example/deploy.git
    path: /opt/deploy
    branch: main
    depth: 1
```

`pulled` — держать рабочую копию синхронной с remote; `register` — чтобы
рестартить сервис только при сдвиге `HEAD`:

```yaml
- name: Keep deploy repo up to date
  module: core.git.pulled
  register: deploy_repo
  params:
    repo: https://github.com/example/deploy.git
    path: /opt/deploy
    branch: main
```

(в [`examples/`](../../../../examples/) задач с `core.git` пока нет — пример минимальный.)

## Безопасность

- **`clone`/`pull` исполняют код из репозитория — главный риск модуля.** Системный
  `git` при checkout прогоняет хуки репозитория (`.git/hooks/*`: `post-checkout`,
  `post-merge` и т.п.), а transport-параметры могут запустить произвольную команду
  на хосте. Модуль вызывает `git clone` / `git pull --ff-only` через подпроцесс
  ([`runClone` / `runPull`](../../../../soul/internal/coremod/git/git.go)) и **не**
  отключает хуки и не песочит git. Следствие: **`repo` обязан указывать на
  доверенный источник** — клонирование недоверенного репозитория = исполнение
  кода его автора с привилегиями процесса `soul`. Аутентификация и transport
  (ssh-agent, credential helper, `~/.netrc`) — на стороне системного `git`, модуль
  их не настраивает.
- **Argument-injection guard `--` есть, но прикрывает только `repo`/`path`.**
  Перед позиционными аргументами стоит разделитель `--`
  (`args = append(args, "--", repo, path)`,
  [`runClone`](../../../../soul/internal/coremod/git/git.go)): `repo`, начинающийся
  с `-` (например `--upload-pack=<cmd>`), не распарсится git как опция. Однако
  `branch` подставляется в `--branch <branch>` **до** `--` и без валидации
  (`OptStringParam`, default `main`): недоверенное значение в `branch` может
  поменять смысл вызова. Держите `branch` под контролем автора Destiny/scenario,
  как и `repo`.
- **Опасно vs. правильно.** Подстановка недоверенного источника в `repo`:

  ```yaml
  # ОПАСНО: repo из внешнего ввода → клонируется чужой репозиторий, его
  # .git/hooks исполнятся при checkout под привилегиями soul-агента.
  - name: Clone user-supplied repo
    module: core.git.cloned
    params:
      repo: "${ input.user_repo_url }"
      path: /opt/app
  ```

  ```yaml
  # БЕЗОПАСНО: repo — фиксированный доверенный адрес, автор Destiny отвечает
  # за его содержимое; branch тоже литерал.
  - name: Clone vetted deploy repo
    module: core.git.cloned
    params:
      repo: https://github.com/example/deploy.git
      path: /opt/deploy
      branch: main
  ```

- **Привилегии.** Модуль **не** объявляет `run_as_root` — в манифесте
  ([`git.yaml`](../../../../shared/coremanifest/git.yaml)) только
  [`exec_subprocess`](../../../naming-rules.md#required_capabilities-enum) (вызов
  `git`) и [`network_outbound`](../../../naming-rules.md#required_capabilities-enum)
  (clone/pull ходят на remote). Файловая запись и сам `git` идут с привилегиями
  процесса `soul`-агента; запись в системные пути (`/opt/...`, `/etc/...`) на
  практике требует root — тогда хуки недоверенного репо исполнятся под root, что
  усиливает цену доверия к `repo`, а не смягчает её.
- **`pull` — только fast-forward** (`--ff-only`,
  [`runPull`](../../../../soul/internal/coremod/git/git.go)): расходящаяся локальная
  история не мёржится силой, шаг падает. Это защита от тихой потери локальных
  коммитов на хосте, а не security-граница против недоверенного remote.

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
