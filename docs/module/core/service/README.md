# core.service

Управление сервисами OS (активность + автозапуск). **Soul-side**, статически
встроен в `soul`-бинарь. Реализация — [`soul/internal/coremod/service/service.go`](../../../../soul/internal/coremod/service/service.go).

Backend берётся из soulprint-факта `os.init_system` (**primary**, [ADR-018(b)](../../../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp));
**fallback** при пустом/unknown факте — рантайм-детект `util.DetectInitSystem`: **systemd**
(`systemctl --version`) → **openrc** (`rc-service --version`) → **sysv**
(`service --version`), в этом порядке. systemd проверяется первым: `systemctl
--version` отрабатывает и на minimal-системах, где systemd установлен, но не PID 1
(chroot/container) — модуль идёт в systemd-ветку. Если ни факт, ни детект не дали init —
шаг падает (`no supported init system detected (systemd/openrc/sysv)`).

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `running` | Сервис активен. Опциональный param `enabled` (tri-state) одним шагом управляет автозапуском. | `changed=true`, если сервис был неактивен и запущен, ИЛИ если `enabled` задан и autostart-состояние пришлось изменить. Уже активен и (при `enabled`) autostart совпадает — `changed=false`. |
| `stopped` | Сервис остановлен. | `changed=true`, если был активен и остановлен. Уже неактивен — `changed=false`. |
| `restarted` | Безусловный restart. | **Всегда `changed=true`** — пользователь явно попросил рестарт (например после изменения конфига). Идемпотентности здесь нет намеренно. |
| `enabled` | Автозапуск при загрузке системы (ortho к активности). | `changed=true`, если autostart был выключен и включён. Уже включён — `changed=false`. |

Мутирующие states (`running` / `restarted` / `enabled`, **НЕ** `stopped`) перед своим
action на systemd-backend выполняют `systemctl daemon-reload` — поведением управляет
опциональный param [`daemon_reload`](#daemon_reload--перечитывание-unit-файлов) (см. ниже).
Сам reload **не** влияет на `changed` шага.

## running — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя сервиса/юнита. |
| `enabled` | bool | optional (tri-state) | Управление автозапуском **одним шагом** (параллель Ansible `service state=started enabled=yes`): **опущено** — autostart не трогаем (управляем только активностью); **`true`** — дополнительно `enable`; **`false`** — дополнительно `disable`. enable/disable идемпотентны (сверка через `is-enabled`). |
| `daemon_reload` | string enum | optional, default `auto` | `auto` \| `always` \| `never`. Управляет `systemctl daemon-reload` перед start (systemd). См. [§ daemon_reload](#daemon_reload--перечитывание-unit-файлов). |

## stopped — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя сервиса/юнита. |

## restarted — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя сервиса/юнита. (`enabled` для `restarted` не применяется — state только рестартит.) |
| `daemon_reload` | string enum | optional, default `auto` | `auto` \| `always` \| `never`. Управляет `systemctl daemon-reload` перед restart (systemd). См. [§ daemon_reload](#daemon_reload--перечитывание-unit-файлов). |

## enabled — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя сервиса/юнита. |
| `daemon_reload` | string enum | optional, default `auto` | `auto` \| `always` \| `never`. Управляет `systemctl daemon-reload` перед enable (systemd). См. [§ daemon_reload](#daemon_reload--перечитывание-unit-файлов). |

## daemon_reload — перечитывание unit-файлов

Опциональный param на states `running` / `restarted` / `enabled` (на `stopped`
**не** объявлен — там reload не нужен). Управляет тем, перечитает ли systemd unit-файлы
(`systemctl daemon-reload`) **перед** мутирующим action.

**Зачем.** systemd держит unit-определения в памяти. После того как unit-файл (или
drop-in) на диске изменили — например `core.file.rendered` отрендерил новый
`redis-server.service.d/hardening.conf` — старое определение в памяти ещё актуально,
пока не сделан `daemon-reload`. Если в этот момент вызвать `systemctl restart`, systemd
**тихо рестартует сервис со СТАРЫМ unit-определением** (команда отрабатывает с exit 0,
лишь warning `Unit file changed on disk, recommend reloading`) — изменения unit-файла
не применяются, и расхождение «на диске одно, в памяти другое» можно не заметить. Чтобы
не приходилось вручную ставить `core.exec.run systemctl daemon-reload` перед каждым
`restarted` (см. [прод-конвенции §3a](../../../destiny/production-conventions.md)),
`core.service` делает reload сам.

**Значения** (`string`, closed-set; неизвестное значение отвергается на Validate, не молча):

| Значение | Поведение (systemd-backend) |
|---|---|
| `auto` (**default**) | Gated по systemd-флагу: модуль читает `systemctl show <unit> --property=NeedDaemonReload --value`; если `yes` — делает `systemctl daemon-reload`, иначе ничего. Идемпотентно: reload только при реальном рассинхроне unit-файла с загруженным определением. |
| `always` | `systemctl daemon-reload` безусловно перед action. |
| `never` | Явный opt-out — reload не делается никогда. |

**Граничные случаи:**

- **Первый install нового unit-файла** → `NeedDaemonReload=no` → при `auto` reload
  **не** выполняется (это корректно: systemd подхватит ещё не загруженное определение
  на самом `start`). reload нужен именно при *изменении* уже загруженного unit-а.
- **non-systemd init** (`openrc` / `sysv` / launchd) — daemon-reload **no-op** при любом
  значении param: у этих init-систем нет аналога `daemon-reload`.
- **reload НЕ влияет на `changed` шага.** `changed` остаётся функцией только
  start/restart/enable (для `restarted` — всегда `true`). reload — это побочное условие
  применения, не самостоятельное изменение состояния сервиса. Факт реально выполненного
  reload отражается **только** диагностическим output-полем `reloaded: true` (см.
  [Output / register](#output--register)).

Existing-задачи без `daemon_reload` получают `auto` — additive и обратно совместимо.

## Capabilities / side-effects

- **Требует root** (`run_as_root`): start/stop/restart/enable/disable через
  init-систему.
- **Выполняет подпроцессы** (`exec_subprocess`): детект init
  (`systemctl`/`rc-service`/`service` `--version`), проверки состояния
  (`is-active`/`is-enabled`, `rc-service status`, `rc-update show`, `chkconfig
  --list`), daemon-reload (`systemctl show <unit> --property=NeedDaemonReload
  --value` при `auto`, затем `systemctl daemon-reload`) и действия
  (`systemctl start|stop|restart|enable|disable`, `rc-service`/`rc-update`,
  `service`/`chkconfig`).
- **Меняет систему:** активность сервиса и/или его autostart.

## Output / register

`running` отдаёт `{ name, active: true }` (+ `enabled: <bool>`, если param
`enabled` был задан). `stopped` — `{ name, active: false }`. `restarted` —
`{ name, active: true }`. `enabled` — `{ name, enabled: true }`.

Если на этом шаге реально выполнился `daemon-reload` (см.
[§ daemon_reload](#daemon_reload--перечитывание-unit-файлов)), в output добавляется
диагностическое поле `reloaded: true`. Поле появляется **только** при фактически
выполненном reload (на `stopped`, на non-systemd, при `never` и при `auto` без
`NeedDaemonReload` его нет) и предназначено для диагностики — на `changed` шага оно
не влияет.

## Примеры

`running` + `enabled` одним шагом, затем реактивный `restarted` через `onchanges`
на изменение конфига/unit-а:

```yaml
- name: Ensure node_exporter is running and enabled at boot
  module: core.service.running
  params:
    name: node_exporter
    enabled: true

- name: Restart node_exporter because unit changed
  module: core.service.restarted
  onchanges: [node_exporter_unit]
  timeout: 30s
  params:
    name: node_exporter
```

(дидактический срез связки `running` + `onchanges`-`restarted`; аналогичная связка для redis-server — в [`examples/destiny/redis-single/tasks/main.yml`](../../../../examples/destiny/redis-single/tasks/main.yml))

`onchanges:` принимает **список** register-ов — рестарт триггерится, если изменился **хотя бы один** из них. Так демон перезапускается и на изменение unit-файла, и на апгрейд бинаря:

```yaml
# node_exporter_unit — register render-задачи unit-а;
# node_exporter_bin  — register version-aware install-задачи (core.cmd.shell с unless).
# Апгрейд версии (install changed) рестартит сервис так же, как правка unit-а.
- name: Restart node_exporter because unit or binary changed
  module: core.service.restarted
  onchanges: [node_exporter_unit, node_exporter_bin]
  timeout: 30s
  params:
    name: node_exporter
```

(из [`examples/destiny/node-exporter/tasks/service.yml`](../../../../examples/destiny/node-exporter/tasks/service.yml) — фактический рестарт-шаг эталона объединяет оба register-а в один `onchanges`-список)

В обоих примерах `daemon_reload` не задан → действует `auto`: если `onchanges`-триггер
был изменением unit-файла, systemd увидит `NeedDaemonReload=yes` и модуль сам перечитает
unit перед рестартом. Отдельный шаг `core.exec.run systemctl daemon-reload` перед
`restarted` для этого больше не нужен (см.
[прод-конвенции §3a](../../../destiny/production-conventions.md)). Безусловный reload —
`daemon_reload: always`; отключить — `daemon_reload: never`:

```yaml
- name: Restart redis after drop-in change (force daemon-reload)
  module: core.service.restarted
  onchanges: [redis_hardening_dropin]
  params:
    name: redis-server
    daemon_reload: always
```

## Безопасность

- **Модуль не исполняет произвольный код, но изменение состояния сервиса —
  реальный side-effect, который может уронить доступность хоста.** `core.service`
  лишь вызывает команды init-системы
  (`systemctl start|stop|restart|enable|disable` и эквиваленты OpenRC/sysv,
  [`svcAction`/`enable`/`disable`](../../../../soul/internal/coremod/service/service.go))
  — он **не** запускает shell и не передаёт произвольную строку. Однако `stopped`
  останавливает сервис, а `restarted` **всегда** рестартит (намеренно не
  идемпотентен): на критичном сервисе это управляемый, но реальный обрыв
  обслуживания. Имя `name` должно приходить от автора Destiny/scenario — остановка
  не того юнита из недоверенного `name` = отказ в обслуживании.
- **Что именно стартует — определяет unit-файл, а не модуль.** `running`/`enabled`
  запускают и ставят в автозапуск юнит с именем `name`; **какой код** при этом
  исполнится, задаётся unit-файлом сервиса на хосте (его `ExecStart` и т.п.).
  Связка с [`core.file`](../file/README.md) опасна: если автозапускаемый юнит или
  бинарь под ним пишется недоверенным `core.file`-шагом, то `core.service.enabled`
  закрепляет его исполнение при каждой загрузке. Контролируйте источник unit-файла
  ровно как источник любого исполняемого артефакта.
- **`enabled` ortho активности.** Параметр/state `enabled` управляет автозапуском
  и идемпотентен (сверка через `is-enabled`), но `enable` сам по себе сервис не
  стартует, а `disable` не останавливает уже запущенный — это разные оси. Не
  полагайтесь на `disabled` как на гарантию, что сервис сейчас не работает
  (нужен `stopped`).
- **Привилегии.** Манифест
  [`service.yaml`](../../../../shared/coremanifest/service.yaml) объявляет
  `required_capabilities: [run_as_root, exec_subprocess]` —
  start/stop/restart/enable/disable через init-систему всегда требуют root и
  запуска подпроцессов (`systemctl`/`rc-service`/`service`/`rc-update`/`chkconfig`,
  плюс детект init и проверки `is-active`/`is-enabled`). Это **декларация** для
  статической сверки `soul-lint` с `allowed_capabilities` хоста (см.
  [docs/keeper/plugins.md →
  required_capabilities](../../../keeper/plugins.md#required_capabilities-таблица)),
  а **не** runtime-повышение прав: операция исполняется с привилегиями процесса
  `soul`-агента (под root), без повышения прав внутри модуля.

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [core/file/README.md](../file/README.md) — `core.file.rendered` (типичный источник `onchanges:` для `restarted`).
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP; **Amendment 2026-06-18** — централизованный daemon-reload (`daemon_reload`).
- [destiny/production-conventions.md §3a](../../../destiny/production-conventions.md) — ручной daemon-reload-паттерн (теперь покрыт встроенным `auto`).
- [ADR-018(b)](../../../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp) — `SoulprintFacts.os.init_system` как **primary** источник backend-а; рантайм-детект — fallback.
