# core.firewall

Управление **одним** правилом файрвола (порт / протокол / источник / действие).
**Soul-side**, статически встроен в `soul`-бинарь. Реализация —
[`soul/internal/coremod/firewall/firewall.go`](../../../../soul/internal/coremod/firewall/firewall.go)
(диспетчер + парсинг params),
[`soul/internal/coremod/firewall/ufw.go`](../../../../soul/internal/coremod/firewall/ufw.go)
(backend ufw) и
[`soul/internal/coremod/firewall/firewalld.go`](../../../../soul/internal/coremod/firewall/firewalld.go)
(backend firewalld).

Backend определяется автоматически (`util.DetectFirewall`) по установленному
управляющему бинарю, **не** по Soulprint: **ufw** (проверяется первым — чаще на
debian-парке), затем **firewalld** (`firewall-cmd`, redhat-парк). Если ни один не
найден — шаг падает (`no supported firewall detected (ufw/firewalld)`).
**iptables сознательно отложен** (требует chain-семантику и `ip(6)tables-save`,
не покрываемые парой add/delete одного правила).

## Безопасность

Модуль работает **только** с конкретным правилом (add/delete). Он **никогда** не
трогает default policy и **никогда** не включает файрвол целиком: нет ни
`ufw enable`, ни `systemctl start firewalld`, ни правок `ufw default` / target
зоны ([ADR-016](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) «безопасность на первом месте»).
Включение файрвола с дефолтной deny-политикой на удалённом хосте мгновенно
отрезало бы SSH и потеряло управление. Это покрыто unit-тестом: `Apply` не
должен генерировать ни одной enable/default-команды.

Для firewalld мутации идут через `--permanent` + явный `firewall-cmd --reload`
(правило переживает рестарт; `--reload` применяет permanent-конфиг в runtime,
**не** перезапускает службу и **не** меняет default policy).

- **Привилегии.** Манифест
  [`firewall.yaml`](../../../../shared/coremanifest/firewall.yaml) объявляет
  `required_capabilities: [run_as_root, exec_subprocess]` — правка правил
  файрвола требует UID 0 и идёт через подпроцессы `ufw` / `firewall-cmd`
  (status/list + add/delete + `--reload`). Это **декларация** для статической
  сверки `soul-lint` с `allowed_capabilities` хоста (см. [docs/keeper/plugins.md →
  required_capabilities](../../../keeper/plugins.md#required_capabilities-таблица)),
  а **не** runtime-повышение прав: backend-вызовы идут с привилегиями процесса
  `soul`-агента (под root), повышения прав внутри модуля нет.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` | Правило существует. | `changed=true`, если правила не было и оно добавлено. Если уже присутствует — `changed=false` (сверка по парсингу `ufw status` / `firewall-cmd --list-ports`/`--list-rich-rules`). |
| `absent` | Правило удалено. | `changed=true`, если правило было и удалено. Если его нет — `changed=false`. |

## params

Одинаковы для `present` и `absent`.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `port` | int / string | required | Порт `1..65535`. Принимается числом или строкой (на случай `${…}`-интерполяции, дающей строку). |
| `proto` | string | optional (default `tcp`) | Протокол: `tcp` \| `udp`. |
| `action` | string | optional (default `allow`) | Действие: `allow` \| `deny`. |
| `source` | string | optional (default — любой источник) | IPv4-CIDR (`192.168.0.0/24`) или одиночный IPv4 (`10.0.0.1`). Нормализуется к канонической форме: одиночный IP → `/32`, CIDR с host-битами схлопывается к адресу сети. **IPv6 не поддерживается в MVP** — отвергается на `Validate` (оба backend-а работают только с IPv4, тихий приём IPv6 дал бы зацикленный add/drift). |
| `zone` | string | optional (default — default-зона) | Зона firewalld. Для ufw игнорируется. |

## Семантика backend-ов

- **ufw.** `present`/`absent` транслируются в `ufw allow|deny …` и
  `ufw delete allow|deny …`. Без `source` используется краткая форма
  (`80/tcp`), с `source` — развёрнутая (`proto tcp from <src> to any port <n>`),
  симметрично тому, как правило печатается в `ufw status`. Идемпотентность —
  парсинг табличного `ufw status` (учитываются direction-токены `IN`/`OUT`;
  IPv6-зеркала `(v6)` игнорируются).
- **firewalld.** Простое `allow`-правило без `source` — через `--add-port` /
  `--remove-port`, наличие проверяется в `--list-ports`. Правило с `source`
  **или** `action: deny` требует rich-rule (простой port — всегда accept),
  наличие проверяется в `--list-rich-rules`. `deny` → rich-rule с `reject`.

Парсинг вывода CLI хрупок между версиями инструментов — покрыт строгими
unit-тестами на зафиксированных образцах.

## Capabilities / side-effects

- **Требует root** (`run_as_root`): правка правил файрвола.
- **Выполняет подпроцессы** (`exec_subprocess`): `ufw` / `firewall-cmd`
  (status/list + add/delete + `--reload` для firewalld).
- **Меняет систему:** набор правил файрвола (side-effect `port`). Default policy и
  состояние «включён/выключен» — **не трогает** (см. инвариант безопасности).

## Output / register

`{ changed, backend, port, proto, action }`, где `backend` — `ufw` \|
`firewalld`. Поля `source` и `zone` присутствуют в output только если были
заданы (и `source` — уже в нормализованной форме).

## Пример

```yaml
- name: Allow PostgreSQL from internal subnet
  module: core.firewall.present
  params:
    port: 5432
    proto: tcp
    action: allow
    source: 10.0.0.0/8
```

(минимальный валидный пример — в `examples/` задач для `core.firewall` пока нет)

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
- [ADR-016](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) — «безопасность на первом месте» (инвариант default policy).
