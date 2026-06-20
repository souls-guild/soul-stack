# core.cmd

Запуск shell-строки через `sh -c "<cmd>"` — с обработкой pipes, redirects, glob,
переменных. **Soul-side**, статически встроен в `soul`-бинарь. Реализация —
[`soul/internal/coremod/cmd/cmd.go`](../../../../soul/internal/coremod/cmd/cmd.go).

Это verb-модуль: единственное состояние — `shell`. В отличие от
[`core.exec`](../exec/README.md) (argv, без shell) здесь строка интерпретируется
shell-ом. **Модуль TRUSTED-ONLY**: `cmd`-строка уходит в `sh -c` без escape —
это shell by design; любая интерполяция (CEL-render, register, soulprint) внутри
`cmd` исполняется shell-ом как код, поэтому источник строки должен быть
доверенным (автор Destiny/scenario), а не внешним вводом. Где shell-семантика не
нужна — используйте `core.exec`.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `shell` | Выполнить `sh -c "<cmd>"`. | По умолчанию `changed=true` (verb «выполнить»). Понизить до no-op можно guard-параметрами `creates` / `unless` / `onlyif` (порядок проверки: creates → unless → onlyif, первый сработавший выигрывает): при срабатывании команда **не** запускается, `changed=false`, output `{ skipped: true, reason, exit_code: 0 }`. |

## shell — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `cmd` | string | required | Shell-строка. Выполняется как `sh -c "<cmd>"`; pipes, redirects, glob, подстановки работают. |
| `cwd` | string | optional | Рабочий каталог процесса `sh`. |
| `env` | map&lt;string,string&gt; | optional | Переменные окружения процесса (`KEY=VALUE`). |
| `creates` | string | optional | Guard: если файл по этому пути **существует** — пропуск (`changed=false`, `reason: creates`). |
| `unless` | string | optional | Guard: выполнить `sh -c "<unless>"`; если её exit **= 0** — пропуск (`reason: unless`). |
| `onlyif` | string | optional | Guard: выполнить `sh -c "<onlyif>"`; если её exit **≠ 0** — пропуск (`reason: onlyif`). |

## Capabilities / side-effects

- **Выполняет подпроцессы** ([`exec_subprocess`](../../../naming-rules.md#required_capabilities-enum)):
  основная команда (`sh -c "<cmd>"`), а также guard-команды `unless` / `onlyif`
  (тоже через `sh -c`).
- **Меняет систему ровно настолько, насколько меняет shell-строка** — модуль сам
  по себе ничего не пишет. Для системных операций требует соответствующих прав
  (на практике — root, [`run_as_root`](../../../naming-rules.md#required_capabilities-enum)).
- `creates` использует `os.Stat` (существование пути), `unless`/`onlyif` —
  вспомогательные shell-вызовы.

## Output / register

`shell` отдаёт `{ stdout, stderr, exit_code }` (exit_code — число). При
срабатывании guard-а — `{ skipped: true, reason, exit_code: 0 }` с `changed=false`.
Как и у `core.exec`, non-zero exit основной команды сам по себе не делает шаг
failed — решает `failed_when:` в scenario.

## Примеры

**`creates`-guard** — install пропускается, если файл-результат уже на месте (простейшая идемпотентность, но **не** ловит апгрейд версии: путь тот же → шаг no-op даже когда содержимое устарело):

```yaml
# Разложить бинарь из распакованного каталога: install -m 0755 — локальный shell,
# без сети. Идемпотентность — через creates: бинарь на месте → no-op.
- name: Install redis_exporter binary
  module: core.cmd.shell
  params:
    creates: "${ input.bin_dir + '/redis_exporter' }"
    cmd: >-
      install -m 0755
      '${ '/tmp/redis_exporter-' + input.redis_exporter_version + '/redis_exporter-v' + input.redis_exporter_version + '.linux-' + soulprint.self.os.arch + '/redis_exporter' }'
      '${ input.bin_dir + '/redis_exporter' }'
```

(из инлайн-блока redis_exporter в [`examples/service/monitoring/scenario/create/main.yml`](../../../../examples/service/monitoring/scenario/create/main.yml). Эталонный `node-exporter` install-шаг идёт **не** через `creates`, а version-aware `unless` — см. ниже: `creates` не ловит апгрейд версии, поэтому бинарь под пин-версию лучше ставить через `unless`)

**Version-aware `unless`-guard** — install пропускается ТОЛЬКО когда на месте уже стоит бинарь нужной версии. В отличие от `creates`, это позволяет **апгрейд**: другая версия → `unless` не satisfied → install выполняется и перезаписывает старый бинарь. `unless` satisfied = exit 0 (`--version`-вывод содержит ожидаемую версию):

```yaml
# install пропускается, только если уже стоит node_exporter нужной версии;
# любая другая версия (или отсутствие бинаря) → unless не satisfied → переустановка.
# register: install-шаг подключён к onchanges рестарта — апгрейд рестартит сервис.
- name: Install node_exporter binary
  module: core.cmd.shell
  register: node_exporter_bin
  params:
    unless: "${ 'test -x ' + input.bin_dir + '/node_exporter && ' + input.bin_dir + '/node_exporter --version 2>&1 | grep -qF ' + \"'version \" + input.version + \" '\" }"
    cmd: >-
      install -m 0755
      '${ '/tmp/node_exporter-' + input.version + '/node_exporter-' + input.version + '.linux-' + soulprint.self.os.arch + '/node_exporter' }'
      '${ input.bin_dir + '/node_exporter' }'
```

(из [`examples/destiny/node-exporter/tasks/service.yml`](../../../../examples/destiny/node-exporter/tasks/service.yml). `grep`-паттерн `'version <X> '` — в одинарных кавычках, с пробелами по обе стороны версии. `node_exporter --version` печатает строку `node_exporter, version <X> (...)` — пробел перед `(` есть всегда; ведущий пробел отделяет токен `version`, **trailing-пробел обязателен**, чтобы паттерн `'version 1.9.0 '` НЕ дал ложный матч на выводе `version 1.9.01 ` (без trailing-пробела `grep -qF 'version 1.9.0'` совпал бы и с `1.9.01`). `input.version` под semver-`pattern`, в кавычках injection невозможна. `arch` берётся из `soulprint.self.os.arch` — стабильный self-факт хоста, доступный в CEL-проходе destiny.)

## Безопасность

- **TRUSTED-ONLY — главный инвариант модуля.** `cmd`-строка уходит в shell как
  `sh -c "<cmd>"` без какого-либо escape (`util.RunOpts{Name: "sh", Args:
  ["-c", shellCmd]}`, [`cmd.go`](../../../../soul/internal/coremod/cmd/cmd.go)).
  Это shell by design: pipes/redirects/glob/подстановки нужны самому модулю.
  Следствие — **любая интерполяция недоверенного в `cmd` = shell-injection**.
  Значения из CEL-render, `register.*`, `soulprint.*`, `input.*` попадают в строку
  и исполняются shell-ом как код через метасимволы `$`, `` ` ``, `|`, `&`, `;`,
  `>`, `<`, `(`, `*`. Источник `cmd`-строки должен быть автором Destiny/scenario,
  а не внешним вводом. Те же guard-команды `unless` / `onlyif` тоже идут через
  `sh -c` — на них распространяется ровно тот же запрет на недоверенную
  интерполяцию.
- **Привилегии.** Модуль **не** объявляет `run_as_root` — в манифесте
  ([`cmd.yaml`](../../../../shared/coremanifest/cmd.yaml)) только
  [`exec_subprocess`](../../../naming-rules.md#required_capabilities-enum).
  Команда исполняется с привилегиями процесса `soul`-агента, без повышения прав
  внутри модуля; для системных операций агент на практике работает под root, и
  тогда `sh -c` тоже идёт под root — это усиливает цену injection, а не смягчает.
- **Опасно vs. правильно.** Подстановка недоверенного значения прямо в shell-строку:

  ```yaml
  # ОПАСНО: filename из недоверенного источника интерпретируется shell-ом.
  # filename = "x; rm -rf /var/lib/app" → выполнится rm.
  - name: Remove uploaded file
    module: core.cmd.shell
    params:
      cmd: "rm -f /srv/uploads/${ input.filename }"
  ```

  Если shell-семантика (pipes/redirects/glob) не нужна — переписать на
  [`core.exec.run`](../exec/README.md), где argv-форма передаёт значение отдельным
  токеном и метасимволы **не** интерпретируются (сверено: `core.exec` запускает
  `exec.CommandContext(cmd, args...)` без `sh -c`):

  ```yaml
  # БЕЗОПАСНО: filename — отдельный argv-токен, shell не участвует.
  - name: Remove uploaded file
    module: core.exec.run
    params:
      cmd: rm
      args: ["-f", "/srv/uploads/${ input.filename }"]
  ```

  Если shell-семантика действительно нужна и часть `cmd` приходит из CEL-render —
  значение обязано квотироваться helper-ом `${ q(...) }` (квотинг для shell,
  **post-MVP**: пока недоступен — см. package-doc
  [`cmd.go`](../../../../soul/internal/coremod/cmd/cmd.go); до его появления
  держите такие шаги полностью под контролем автора Destiny).

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [core/exec/README.md](../exec/README.md) — argv-вариант без shell (TRUSTED-ONLY не нужен); те же guard-флаги.
- [core/archive/README.md](../archive/README.md) — распаковка перед install-шагом.
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
