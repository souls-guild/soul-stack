# `input:` в destiny

Этот документ описывает **destiny-специфику** блока `input:`. Сам формат — общий стандарт для destiny / scenario / манифеста модуля и описан в [`docs/input.md`](../input.md). Здесь — где `input:` валидируется, как используется внутри destiny и какие специфичные правила и подсказки применяются к destiny-параметрам.

## Источник правды на формат

Точные ключи (`type`, `enum`, `pattern`, `format`, `min_length`, `secret`, …), типы (`string`, `integer`, `number`, `boolean`, `array`, `object`) и правила валидации — в [`docs/input.md`](../input.md). При расхождениях приоритет за тем документом. Любой новый ключ — propose-and-wait → правка [`docs/input.md`](../input.md) → потом этот файл и примеры.

## Где блок живёт

В корне `destiny.yml` (см. [manifest.md](manifest.md) → поле `input:`). Не в `tasks/main.yml`. Один destiny — один блок `input:`. Все задачи `tasks/main.yml` читают значения из общего набора параметров.

## Где валидируется

Defense in depth — два независимых раунда валидации, оба обязательны:

1. **Keeper при инвокации.** Когда scenario или прямой API-вызов запускает destiny, Keeper читает `input:` destiny и проверяет переданные значения **до** того, как что-либо уйдёт на Souls. Ошибка → fail fast, диагностика оператору, нулевой трафик к хостам.
2. **Soul перед apply.** Получив destiny + значения от Keeper-а (или от `keeper.push`), Soul валидирует их повторно. Страхует от рассинхронизации версий, ручной инъекции и багов в Keeper-е.

См. также [`docs/input.md`](../input.md) — общий стандарт формата (где `input:` живёт в каждом артефакте).

## Как используется внутри destiny

В `tasks/main.yml`, в шаблонах `templates/*.tmpl` и в условиях `when:` значения referenced как `input.<name>` (через `${ ... }` в строковой интерполяции, голая форма в top-level expression-keys — см. [`docs/templating.md`](../templating.md)):

```yaml
# destiny.yml
input:
  action:
    type: string
    required: true
    enum: [apply, restart, ping]
  version:
    type: string
    format: semver

# tasks/main.yml
tasks:
  - name: Install redis-server package
    module: core.pkg.installed
    when: input.action == 'apply'
    params:
      name: redis-server
      version: "${ input.version }"

  - name: Restart redis-server
    module: core.service.restarted
    when: input.action == 'restart'
    params:
      name: redis-server
```

## `input.<name>` vs `params:` задачи

Не путать:

| | `input.<name>` | `params.<name>` |
|---|---|---|
| **Что** | Параметр destiny, объявленный в `destiny.yml → input:` | Аргумент конкретного модуля, передаваемый в шаге `tasks/main.yml` |
| **Откуда схема** | [`docs/input.md`](../input.md) — общий стандарт | Манифест модуля для конкретного состояния (см. [architecture.md → «Манифест модуля»](../architecture.md#манифест-модуля)) |
| **Кто валидирует** | Keeper + Soul (см. выше) | Soul при apply; `soul-lint` статически по манифесту модуля |
| **Доступ в шаблонах** | `${ input.action }` (или голая `input.action` в top-level expression-keys) | внутреннее значение задачи, не visible снаружи |

Имена намеренно разные: `input` — *снаружи внутрь destiny*; `params` — *внутрь модуля*.

## Destiny-специфичные правила и подсказки

Базовые подсказки для авторов схем — в [`docs/input.md` → «Подсказки авторам»](../input.md#подсказки-авторам). Дополнения, специфичные для destiny:

- **`action:`-параметр почти всегда есть.** Destiny обычно объявляет верх-уровневый `action: { type: string, enum: [...] }` — он определяет, какие задачи `tasks/main.yml` исполнятся через `when:`. Это та точка, в которую упирается «один destiny — несколько режимов работы» (apply / restart / ping / status-check).
- **Все секреты — через `secret: true`.** Пароли, токены, приватные ключи и vault-ссылки. Без этого значение засветится в логах apply при ошибке задачи; такие инциденты — самый дешёвый класс утечек.
- **Vault-ссылки — `pattern: "^vault:.*"`**, а не «строка как строка». Резолв в реальное значение делает Keeper перед отправкой destiny на Soul; до тех пор destiny оперирует ссылкой, не значением.
- **`enum:` важнее `pattern:`.** Конечный список значений (`apply | restart | ping`) гораздо лучше регулярки `^(apply|restart|ping)$` — лучше читается, валидируется линтером, доступен в UI/MCP-каталоге как dropdown.

## Связь с `input:` scenario

Scenario тоже имеет блок `input:` (того же формата). Но это **разные** контракты:

- Scenario `input:` — что оператор передал при запуске сценария (`keeper.incarnation.run scenario=add_user inputs={...}`).
- Destiny `input:` — что scenario передал destiny через `apply: { destiny: ..., params: { ... } }`.

Scenario вычисляет destiny-`input:` из своего scenario-`input:`, `vars` (essence) и `state` — и передаёт в destiny. Внутри destiny scenario-`input:` **не виден** — destiny знает только то, что пришло на её `input:`.

## См. также

- [`docs/input.md`](../input.md) — общий стандарт формата `input:`.
- [manifest.md](manifest.md) — где `input:` лежит в `destiny.yml`.
- [tasks.md](tasks.md) — как `input.<name>` используется в задачах.
- [architecture.md → «Destiny: входной контракт и валидация»](../architecture.md#destiny-входной-контракт-и-валидация) — раунды валидации и связь с soul-lint.
