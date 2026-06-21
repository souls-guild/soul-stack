# soul-lint

Офлайн-линтер для Destiny и Essence. Назначение и место в системе закреплены в [ADR-004](adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper): разбор, рендер, проверка по схеме, статический анализ. Работает без подключения к Keeper — пригоден для CI, IDE и локального запуска.

Этот документ ведёт список **планируемых проверок** (TODO) и фиксирует правила/обоснования, по которым линтер должен их выполнять.

## Планируемые проверки

### 1. Согласованность литералов в `when` с `enum` входных параметров

**Проблема.** В Destiny условия шага записываются как top-level expression-keys — вся строка трактуется как CEL без обёртки ([ADR-010](adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [`docs/templating.md §2.1`](templating.md#21-top-level-expression-ключи)):

```yaml
input:
  action:
    type: string
    required: true
    enum: [apply, ensure_user, restart, ping, replication_status]

# destiny-<name>/tasks/main.yml:
- module: core.pkg.installed
  when: input.action == 'apply'
  ...
```

Строковый литерал `'apply'` внутри `when:` никак формально не связан с объявленным `enum`. Опечатка (`'aply'`) не вызовет ошибку — задача просто никогда не сработает, и destiny внешне «отработает». Это тихий класс ошибок, который надо ловить статически.

**Что должен делать линтер.**

1. Распарсить выражения в полях `when:` (и в строковых значениях `input.*`, если решим расширить — обсудить отдельно). Парсер использует `cel-go` ([ADR-010](adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [`docs/templating.md`](templating.md)) — тот же движок, что в runtime, поэтому статика и runtime согласованы.
2. Извлечь паттерны вида:
   - `input.X == '<literal>'` / `input.X != '<literal>'`
   - `input.X in [<literal>, <literal>, ...]`
   - аналоги для `==`/`in` через `|` / встроенные функции — по мере фиксации синтаксиса.
3. Проверить по блоку [`input:`](destiny/input.md) destiny:
   - параметр `X` объявлен в `input:`;
   - если у `X` указан `enum` — каждый литерал из выражения принадлежит этому `enum`;
   - в противном случае — `error` с указанием на возможные значения.
4. **Coverage-warning.** Значение `enum`, которое нигде не упомянуто ни в одном `when` destiny, — кандидат на опечатку в `enum:` (либо мёртвая ветка). Уровень: `warn`, не `error`.

**Ограничения.** Проверка покрывает «простые» сравнения. Произвольно сложные выражения (вычисления, конкатенации, вызовы функций над `input.X`) остаются вне проверки — для них допустимы только `warn`-эвристики или явный escape (`# soul-lint:ignore-when`). Это осознанный размен: ловим 90% типичных ошибок, не пытаясь анализировать turing-complete шаблоны.

**Зависимости.**
- Шаблонизатор выражений зафиксирован [ADR-010](adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов): CEL для YAML-выражений, парсер — `cel-go` (см. [`docs/templating.md`](templating.md)). Линтер использует тот же движок — статика и runtime согласованы, иначе расходятся.
- Если в будущем выберем вариант со структурированным matcher-ом (`when: { param: action, equals: apply }`) — часть этой проверки уйдёт в схему destiny и линтер будет нужен только для escape-формы.

## Backlog: проверки scenario (после ADR-009)

Не закреплённый дизайн, фиксируется при реализации scenario-резолва. Введён вместе с [ADR-009](adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) и [ADR-008](adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги).

### B1. Warn: инлайн-мутация в scenario без выноса в destiny

**Проблема.** [ADR-009](adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) разрешил изменяющие `module:`-шаги прямо в scenario, но граница «переиспользуемое/критичное → в destiny» — рекомендация, не запрет. Без подсказки авторы будут инлайнить мутации, минуя независимое git-версионирование destiny ([ADR-007](adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)) — риск эрозии, явно отмеченный в ADR-009.

**Что должен делать линтер.** Эвристически отмечать `module:`-шаги в scenario, чьё `<state>` помечено в манифесте модуля как изменяющее (`side_effects`), и которые не обёрнуты в `apply: { destiny: … }`. Уровень — **`warn`** (не `error`: это рекомендация). Сообщение указывает на критерий выноса из [`docs/scenario/concept.md`](scenario/concept.md) (три «да»). read-only-шаги (probe, `changed_when: false`) под правило **не** попадают.

### B2. Статпроверка `where:`- и `on:`-литералов

**Проблема.** `where:`-предикат ссылается на `register:` предыдущего probe-шага; `on:`-литерал — на coven-имя. Опечатка (`redis_rol.stdout`, несуществующий coven) тихо приводит к пустому таргету — разрушительный шаг «отработает» ни на ком, и это незаметно.

**Что должен делать линтер.**
- В `where:` — проверить, что упомянутые `register`-id объявлены раньше по flow в этом сценарии (как requisite-проверки в destiny). Неизвестный `register` → `error`.
- В `on:` — проверить форму (`keeper` / список coven-литералов / опущен) и, где статически возможно, что coven-литерал не пуст и шаблон синтаксически валиден. Соответствие литерала реальным covens — рантайм (Postgres), статикой не ловится; уровень для подозрительных литералов — `warn`.
- Проверить register-инвариант: **каждый `register.<name>`, упомянутый в `where:`, обязан быть `register:` probe-шага, завершившегося раньше по flow** → иначе `error`. Чисто-стабильный предикат (только `soulprint.self.*`, без единого `register.*`) probe **не требует** и ошибкой не является (см. [`docs/scenario/orchestration.md §4`](scenario/orchestration.md#4-волатильный-предикат--where)).

**Зависимости.** Шаблонизатор зафиксирован [ADR-010](adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) (CEL для top-level expression-keys, `cel-go`-парсер); scenario использует тот же engine ([ADR-009](adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). Resolved-путь двухуровневого резолва ресурсов (локально → service-level) печатается движком и проверяется линтером — см. [`docs/scenario/orchestration.md §6`](scenario/orchestration.md#6-двухуровневый-резолв-ресурсов).

**Статус реализации (текущий MVP).** Cross-ref `register.<name>` против объявлений в плане задачи (включая block-вложенность) — реализован (codes `duplicate_task_address`, `unknown_register_reference`; `register.self.*` исключён). `duplicate_task_address` ловит дубль в адресном пространстве подписки `register ∪ id` (два `register`, два `id`, либо `register` одной задачи == `id` другой; ADR-052 §h, [destiny/tasks.md §8](destiny/tasks.md)) — last-wins-резолв тихо привязал бы зависимость/алерт не к той задаче. Cross-file (дубль адреса между основным файлом и подключённым через `include:`) проверяется на плоском плане после раскрытия include. Дополнительно подключена статпроверка `soulprint.<...>`-ссылок в CEL-предикатах (`where`/`when`/`changed_when`/`failed_when`/`retry.until`/`loop.when`):
- голая `soulprint.<x>` без `.self`/`.hosts`/`.where` → `soulprint_naked_reference` (каноническая форма обязательна, см. [`docs/soul/soulprint.md`](soul/soulprint.md));
- `soulprint.self.<unknown_top>` (опечатка типа `memmory`/`familly` на верхнем сегменте) → `soulprint_unknown_path` со сверкой по typed-схеме [ADR-018](adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp) (`sid`/`hostname`/`os`/`kernel`/`cpu`/`memory`/`network`/`covens`/`role`);
- `soulprint.hosts.where(...)` / `soulprint.where(...)` пропускаются — это scenario-only аксессоры, их валидирует `shared/cel.rewriteHostsWhere` в render-фазе ([`docs/scenario/orchestration.md §4.1`](scenario/orchestration.md));
- вторая ступень `soulprint.self.<msg>.<unknown_field>` (опечатка в подсегменте, например `os.familly`) — **отложена** отдельным слайсом по запросу (текущая проверка ловит только опечатки в первом сегменте; placeholder в коде — `checkSoulprintSubPath`).

Подключён детектор register-зависимости внутри `block:` для staged-render ([ADR-056](adr/0056-staged-render-passage.md)): потомок block, читающий register, эмитнутый соседним потомком **того же** блока, → `within_block_register_dependency` (`error`). Block атомарен по Passage (весь fan-out — один Passage), peer-register доступен только Soul-side ПОСЛЕ probe, а `where`/`params`/`vars`/`apply.input` резолвятся Keeper-side ДО dispatch → отбор хостов по устаревшему register молча (silent-wrong-target). Flow-control `when` (Soul-side per-task gating после FC-5 narrow-fix) сюда **не** входит — within-block `when: register.peer` валиден. Лечится выносом probe и потребителя в разные top-level задачи (тогда стратификация штатно разведёт их по Passage). Тот же код страхует runtime keeper-side. Каталог-описание — [`docs/naming-rules.md → Parser / validation errors`](naming-rules.md). Симметричные stratify/passage-коды (`register_dependency_cycle` — цикл register-зависимости; `cross_passage_requisite_unsupported` — cross-passage requisite без журнала аудита) детектятся при стратификации/рантайме, см. тот же каталог.

Подключён детектор cross-passage flow-control gating ([ADR-056](adr/0056-staged-render-passage.md) amend 2026-06-21, FC-5): задача, гейтящая `when:` / `changed_when:` / `failed_when:` по register, эмитнутому в **более раннем** Passage, → `cross_passage_when_unsupported` (`error`). Flow-control = Soul-side per-task gating ([ADR-012(d)](adr/0012-keeper-soul-grpc.md)) — видит только register **своего** Passage; cross-passage register ему недоступен (другой `ApplyRequest`) → `no such key` молча, задача FAILED. `where:` (Keeper-side targeting) cross-passage умеет, `when:` — нет: асимметрия легитимна. Сам `when` Passage **не расщепляет** (flow-control НЕ passage-определяющий), поэтому register-зависимый `when` обычно едет same-passage с probe и работает; код срабатывает, только когда probe уехал в ранний Passage по **другой** причине (иная задача с `where: register.X`). Лечится: `where:` для cross-task register-таргетинга ИЛИ `register.self` для same-task gating (`register.self` детектором НЕ ловится). Тот же код страхует runtime keeper-side.

Для `on:`-литералов: формат (`kebab-case` / `${ ... }`-CEL / `keeper`) — реализован (codes `enum_invalid`, `name_invalid_format`, `type_mismatch`); хук `CovenLabelValidator` (interface в `shared/config`, no-op по умолчанию) подключён к каждому не-CEL-обёрнутому coven-литералу через `SetCovenLabelValidator`. Реальный справочник covens (Q1b ADR-008-amend) подменит no-op без изменения публичного API; до тех пор линтер не флагает «существование» coven (это runtime).

## Что НЕ относится к soul-lint

Динамический прогон destiny на тестовом стенде, измерение runtime-coverage и верификация сценариев в docker — это **отдельный инструмент**, не часть `soul-lint`. По ADR-004 `soul-lint` строго офлайн и статический. Тема ведётся в [destiny/testing.md](destiny/testing.md).
