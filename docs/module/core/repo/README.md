# core.repo

Управление пакетным репозиторием OS (идея ansible `apt_repository`/`yum_repository`,
переработанная под безопасный декларативный MVP). **Soul-side**, статически встроен
в `soul`-бинарь. Реализация — [`soul/internal/coremod/repo/repo.go`](../../../../soul/internal/coremod/repo/repo.go)
(контракт, валидация, параметры) и [`soul/internal/coremod/repo/backends.go`](../../../../soul/internal/coremod/repo/backends.go)
(apt/dnf/yum/apk-бэкенды, файловые операции).

Backend определяется автоматически (`util.DetectPkgMgr`); если ни один менеджер не
найден — шаг падает (`no supported package manager detected`). Целевые артефакты по
backend-ам:

- **apt** → `/etc/apt/sources.list.d/<name>.list` (one-line формат) + ключ в
  `/etc/apt/keyrings/<name>.gpg`, на который `.list` ссылается через `signed-by=`
  (современный формат, **не** `apt-key` — тот deprecated и кладёт ключ в общий
  trust store без привязки к репозиторию);
- **dnf/yum** → `/etc/yum.repos.d/<name>.repo` (ini-формат);
- **apk** → строка в `/etc/apk/repositories`.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` | Репозиторий объявлен: файл-описание (и для apt — GPG-ключ) на месте с нужным содержимым. | `changed=true`, если целевой файл отсутствовал/отличается побайтово либо (apt с `gpg_key`) ключ отсутствовал/отличается. Всё совпало — `changed=false`. |
| `absent` | Описание репозитория удалено. **GPG-ключ не трогается** — он может использоваться другими репозиториями (ручная чистка ключа — отдельный явный шаг). | `changed=true`, если файл/строка были и удалены. Не было — `changed=false`. |

## present — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя репозитория. Становится именем файла (`<name>.list`/`<name>.repo`), поэтому валидируется: только `[A-Za-z0-9._-]`, без `/`, `\` и `..` (защита от path-traversal — записи вне целевого каталога). |
| `uri` | string | required (для `present`) | Базовый URL репозитория. Допустимы `http://` и `https://` (`file://`/`ftp://`/пустая — ошибка). `http://` легитимен для внутреннего зеркала, но даёт обязательный warning (см. [Безопасность](#безопасность)). |
| `gpg_key` | string | optional | Содержимое GPG-ключа inline (ASCII-armored/PEM или бинарный keyring — пишется как есть). Для apt материализуется в `/etc/apt/keyrings/<name>.gpg` (mode `0644`) и подключается через `signed-by=`; для dnf/yum пишется в `gpgkey=`. Скачивание ключа по URL в MVP **не** реализовано (CEL может подставить содержимое через `${ file(...) }`/`vault`). Критичен для supply-chain. |
| `gpg_check` | bool | optional (default `true`) | Криптопроверка пакетов. `false` — opt-out, разрешён, но даёт обязательный warning (симметрия checksum-opt-out в core.url). Для dnf/yum пишется в `gpgcheck=`. |
| `suite` | string | optional | Suite/дистрибутив (apt: `deb <uri> <suite> <components>`). На dnf/yum/apk не влияет (apk кладёт полный URL в `uri`). |
| `components` | list | optional | Компоненты apt-строки (`main contrib …`). Только apt. |
| `enabled` | bool | optional (default `true`) | Включён ли репозиторий. `false`: для apt/apk строка закомментирована (`# …`), для dnf/yum — `enabled=0`. |

## absent — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя репозитория (тот же набор символов). Для apt/dnf/yum определяет удаляемый файл `<name>.list`/`<name>.repo`. |
| `uri` | string | required **для apk** | apk не хранит имя репо в файле, поэтому удаление матчится по `uri`. Для apt/yum `uri` не нужен (есть файл `<name>`). Без `uri` apk-absent падает (иначе угадывание — риск снести чужую строку). |

(`gpg_check`/`suite`/`components`/`enabled`/`gpg_key` для `absent` не используются.)

## Безопасность

[ADR-016](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack)
«безопасность на первом месте», но баланс с легитимными кейсами — мягче, чем у
core.url, с обязательными warning-ами вместо запретов:

- **`gpg_key` критичен для supply-chain.** Если задан — ключ реально
  материализуется (apt: keyring + `signed-by=`) / прописывается как `gpgkey=`
  (dnf/yum) и участвует в idempotency-сравнении. apt использует современный
  `signed-by=`-keyring (доверие привязано к конкретному репозиторию, не к глобальному
  trust store).
- **`gpg_check=false` разрешён** (opt-out), но Apply возвращает обязательный
  warning в output: «packages will NOT be cryptographically verified».
- **`gpg_check=true` без `gpg_key`** — тоже warning, backend-специфичный: для
  dnf/yum это **сломает установку пакетов** (`gpgcheck=1` без `gpgkey=`); apt/apk
  опираются на свои хранилища доверия.
- **`http://` в `uri` допустим** (внутреннее зеркало), но с обязательным warning
  «traffic is unencrypted».
- **`name` санитизируется** против path-traversal (имя становится именем файла).

## Capabilities / side-effects

- **Выполняет подпроцессы:** только для `util.DetectPkgMgr` (определение backend-а).
  Запись описаний репозитория — in-process, без shell.
- **Меняет файловую систему:** пишет/удаляет файл-описание репозитория и (apt с
  `gpg_key`) keyring. Для системных путей (`/etc/apt`, `/etc/yum.repos.d`,
  `/etc/apk`) требует соответствующих прав. Запись — preserve-by-default
  (`util.AtomicWritePreserving`): права/владелец существующего файла сохраняются.
- **Не выполняет `apt-get update`/`dnf makecache`** — модуль только объявляет
  репозиторий; refresh индекса делает `core.pkg` при установке.

## Output / register

`present`/`absent` отдают `{ name, backend, path, changed }`, где `backend` —
`apt`/`yum`/`apk`, `path` — затронутый файл (`<name>.list`/`<name>.repo` или
`/etc/apk/repositories`). Если были warning-и (opt-out gpg_check / http uri / нет
ключа) — поле `warnings: [...]` со списком строк (попадают в output, а не теряются).

## Пример

`present` (apt) с GPG-ключом — минимальный пример:

```yaml
- name: Declare internal apt repo
  module: core.repo.present
  params:
    name: example-internal
    uri: https://apt.example.com/debian
    suite: bookworm
    components: [main]
    gpg_key: "${ file('files/example.gpg.asc') }"
```

`absent` (apk) — удаление матчится по `uri`:

```yaml
- name: Drop old apk mirror
  module: core.repo.absent
  params:
    name: old-mirror
    uri: https://dl-cdn.alpinelinux.org/alpine/edge/testing
```

(в [`examples/`](../../../../examples/) задач с `core.repo` пока нет — примеры минимальные.)

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [core/pkg/README.md](../pkg/README.md) — установка пакетов из объявленного репозитория.
- [core/url/README.md](../url/README.md) — checksum-opt-out, симметричный gpg-opt-out здесь.
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
- [ADR-016](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) — «безопасность на первом месте».
