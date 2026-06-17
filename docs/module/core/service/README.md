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

## running — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя сервиса/юнита. |
| `enabled` | bool | optional (tri-state) | Управление автозапуском **одним шагом** (параллель Ansible `service state=started enabled=yes`): **опущено** — autostart не трогаем (управляем только активностью); **`true`** — дополнительно `enable`; **`false`** — дополнительно `disable`. enable/disable идемпотентны (сверка через `is-enabled`). |

## stopped — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя сервиса/юнита. |

## restarted — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя сервиса/юнита. (`enabled` для `restarted` не применяется — state только рестартит.) |

## enabled — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя сервиса/юнита. |

## Capabilities / side-effects

- **Требует root** (`run_as_root`): start/stop/restart/enable/disable через
  init-систему.
- **Выполняет подпроцессы** (`exec_subprocess`): детект init
  (`systemctl`/`rc-service`/`service` `--version`), проверки состояния
  (`is-active`/`is-enabled`, `rc-service status`, `rc-update show`, `chkconfig
  --list`) и действия (`systemctl start|stop|restart|enable|disable`,
  `rc-service`/`rc-update`, `service`/`chkconfig`).
- **Меняет систему:** активность сервиса и/или его autostart.

## Output / register

`running` отдаёт `{ name, active: true }` (+ `enabled: <bool>`, если param
`enabled` был задан). `stopped` — `{ name, active: false }`. `restarted` —
`{ name, active: true }`. `enabled` — `{ name, enabled: true }`.

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

(из [`examples/destiny/destiny-node-exporter/tasks/main.yml`](../../../../examples/destiny/destiny-node-exporter/tasks/main.yml); аналогичная связка для redis-server — в [`examples/destiny/destiny-redis-single/tasks/main.yml`](../../../../examples/destiny/destiny-redis-single/tasks/main.yml))

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
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
- [ADR-018(b)](../../../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp) — `SoulprintFacts.os.init_system` как **primary** источник backend-а; рантайм-детект — fallback.
