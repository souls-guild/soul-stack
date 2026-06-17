# core.exec

Запуск процесса напрямую через `exec()` с argv — **без shell** (нет pipes,
redirects, glob, подстановки переменных). **Soul-side**, статически встроен в
`soul`-бинарь. Реализация —
[`soul/internal/coremod/exec/exec.go`](../../../../soul/internal/coremod/exec/exec.go).

Это verb-модуль: единственное состояние — `run` (без declarative-семантики
«привести к состоянию»). Для shell-семантики (pipes/redirects) — [`core.cmd`](../cmd/README.md).
Non-zero exit основной команды **не** считается ошибкой автоматически — что
считать провалом, решает автор через `failed_when:` в scenario (например, `grep`
с exit 1 — норма).

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `run` | Запустить `cmd` с `args` через `exec()`. | По умолчанию `changed=true` (verb «выполнить»). Понизить до no-op можно guard-параметрами `creates` / `unless` / `onlyif` (порядок проверки: creates → unless → onlyif, первый сработавший выигрывает): при срабатывании команда **не** запускается, `changed=false`, output `{ skipped: true, reason, exit_code: 0 }`. Для read-only probe ставьте `changed_when: false` в scenario. |

## run — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `cmd` | string | required | Имя/путь исполняемого файла (argv[0]). Запускается напрямую, без `sh -c`. |
| `args` | list&lt;string&gt; | optional | Аргументы команды (argv[1:]). Каждый элемент передаётся как отдельный токен, без shell-разбора. |
| `cwd` | string | optional | Рабочий каталог процесса. |
| `env` | map&lt;string,string&gt; | optional | Переменные окружения процесса (`KEY=VALUE`). |
| `creates` | string | optional | Guard: если файл по этому пути **существует** — пропуск (`changed=false`, `reason: creates`). |
| `unless` | string | optional | Guard: выполнить `sh -c "<unless>"`; если её exit **= 0** — пропуск (`reason: unless`). |
| `onlyif` | string | optional | Guard: выполнить `sh -c "<onlyif>"`; если её exit **≠ 0** — пропуск (`reason: onlyif`). |

## Capabilities / side-effects

- **Выполняет подпроцессы** ([`exec_subprocess`](../../../naming-rules.md#required_capabilities-enum)):
  основная команда (`cmd args`), а также guard-команды `unless` / `onlyif`
  (последние — через `sh -c`).
- **Меняет систему ровно настолько, насколько меняет запущенная команда** —
  модуль обёртка над процессом и сам по себе ничего не пишет. Для системных
  операций требует соответствующих прав (на практике — root,
  [`run_as_root`](../../../naming-rules.md#required_capabilities-enum)).
- `creates` использует `os.Stat` (существование пути), `unless`/`onlyif` —
  вспомогательные shell-вызовы.

## Output / register

`run` отдаёт `{ stdout, stderr, exit_code }` (exit_code — число). При срабатывании
guard-а — `{ skipped: true, reason, exit_code: 0 }` с `changed=false`. Типичное
использование `register:` — read-only probe (`changed_when: false`) с чтением
`register.<name>.stdout` в последующих `where:` / `failed_when:` / `output:`.

## Пример

```yaml
# Read-only probe: запустить argv без shell, прочитать stdout в register.
- name: Read kernel release
  module: core.exec.run
  register: kernel_release
  changed_when: false
  params:
    cmd: uname
    args: ["-r"]

# Запуск с guard creates: бинарь на месте → no-op.
- name: Initialize data dir once
  module: core.exec.run
  params:
    cmd: /usr/local/bin/app
    args: ["init", "--data-dir", "/var/lib/app"]
    creates: /var/lib/app/.initialized
```

## Безопасность

- **argv-форма, без shell — ключевое отличие от [`core.cmd`](../cmd/README.md).**
  `cmd` запускается напрямую как `exec.CommandContext(cmd, args...)` без `sh -c`
  ([`exec.go`](../../../../soul/internal/coremod/exec/exec.go)). Метасимволы
  (`$`, `` ` ``, `|`, `&`, `;`, `>`, `*`) в `cmd`/`args` **не интерпретируются** —
  каждый элемент `args` передаётся отдельным токеном без shell-разбора. Поэтому
  риск shell-injection отсутствует: значение `"x; rm -rf /"` в `args` — это один
  буквальный аргумент, а не команда.
- **Всё равно TRUSTED-ONLY для имени бинаря.** Отсутствие shell не делает модуль
  безопасным для произвольного недоверенного ввода: `cmd` (argv[0]) задаёт, какой
  исполняемый файл будет запущен, а `args` — с какими аргументами. Недоверенное
  значение в `cmd` = запуск произвольного бинаря; недоверенное в `args` может
  поменять смысл операции (флаги, пути). Значения из `register.*` / `soulprint.*`
  / `input.*` в `cmd`/`args` допустимы, только если им доверяет автор
  Destiny/scenario.
- **Guard `unless` / `onlyif` — это shell.** В отличие от основной команды,
  вспомогательные guard-команды исполняются через `sh -c "<unless|onlyif>"`
  ([`shouldSkip`](../../../../soul/internal/coremod/exec/exec.go)) — на их строки
  распространяется тот же запрет на недоверенную интерполяцию, что и у
  [`core.cmd`](../cmd/README.md). `creates` shell не использует — это `os.Stat`
  по пути.
- **Привилегии.** Модуль **не** объявляет `run_as_root` — в манифесте
  ([`exec.yaml`](../../../../shared/coremanifest/exec.yaml)) только
  [`exec_subprocess`](../../../naming-rules.md#required_capabilities-enum).
  Команда исполняется с привилегиями процесса `soul`-агента, без повышения прав
  внутри модуля; для системных операций агент на практике работает под root.
- **Side-effect `creates:` (idempotency-якорь).** При наличии файла по пути
  `creates` команда **не** запускается (`changed=false`, `reason: creates`).
  Это снижает побочные эффекты повторных прогонов, но `creates` проверяет лишь
  существование пути (`os.Stat`), а не содержимое или успех прошлого запуска —
  не полагайтесь на него как на гарантию корректного состояния, только как на
  guard «уже сделано».

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [core/cmd/README.md](../cmd/README.md) — shell-вариант (`sh -c`, pipes/redirects); те же guard-флаги.
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
- [ADR-008](../../../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) — volatile-роль через inline-probe (`core.exec.run` + `register:` + `where:`).
