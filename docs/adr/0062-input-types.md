# ADR-062. Named input types — переиспользуемые именованные схемы input через `types:` + `$type`

> **Статус: active.** Решение пользователя принято (propose-and-wait закрыт), фиксируется в каноне ДО кода (правило «архитектурное решение в том же ходе»). Wave 1 = механизм на текущем `AclUser {name, perms, state}`; границы MVP — в конце ADR.

**Контекст.** input-DSL ([docs/input.md](../input.md), `config.InputSchema`) описывает контракт входов scenario/destiny/манифеста модуля. Один и тот же составной тип (например запись пользователя `{name, perms, state}` в сервисе `redis`) встречается **в нескольких сценариях** одного сервиса — `add_user`, `update_acl`, `create` (массив таких записей). Сейчас его приходится **дублировать inline** в каждом `input:`. Дубликат расходится: правка в одном месте не доезжает в другие, и `additional_properties: false` / `required` начинают отличаться между сценариями для логически одного типа.

Прежний задел под переиспользование — **`$ref` на внешний JSON-Schema-файл в папке `schemas/`** (`input: { $ref: "../../schemas/user.yaml" }`) — был задекларирован в [architecture.md](../architecture.md) и [service/manifest.md](../service/manifest.md), но **никогда не реализован**. У него три проблемы: (1) вводит **вторую схемную DSL** (внешний JSON Schema рядом с нашим input-DSL — расхождение `properties`/`required`-семантики, `type`-словаря, code-ошибок `input_*`); (2) `$ref` с относительным путём — file-resolution и path-traversal-поверхность; (3) `$ref` под ключом `input:` синтаксически конфликтует с самим input-блоком (что значит `input: {$ref: ...}` — заменить весь блок? один параметр?).

**Решение (зафиксировано пользователем).** Заменить нереализованный `$ref`/`schemas/` на **named input types**: переиспользуемые именованные схемы в **том же input-DSL**, объявленные в service-level файле, и ссылка на них директивой `$type`.

1. **Секция `types:` в service-level файле `service/<name>/types.yml`.** Map `<Имя>` → схема в **том же** InputSchema-DSL (`type`/`properties`/`required`/`items`/`enum`/`pattern`/`format`/… — весь словарь [docs/input.md](../input.md), включая вложенность). Никакого внешнего JSON Schema. Имя типа — `PascalCase` (`^[A-Z][A-Za-z0-9]*$`), отличает тип-ссылку от параметра-имени (snake_case) визуально и в парсере.

   ```yaml
   # service/redis/types.yml
   types:
     AclUser:
       type: object
       additional_properties: false
       required: [name, perms, state]
       properties:
         name:  { type: string, pattern: "^[a-zA-Z0-9_-]+$" }
         perms: { type: string }
         state: { type: string, enum: [on, off] }
   ```

2. **Ссылка `$type: <Имя>` как самостоятельное поле** ИЛИ **`items: {$type: <Имя>}`** для массива таких элементов:

   ```yaml
   # scenario/add_user/main.yml
   input:
     user:
       $type: AclUser            # одиночный объект объявленного типа

   # scenario/create/main.yml
   input:
     users:
       type: array
       items:
         $type: AclUser          # массив объявленного типа
       min_items: 1
   ```

   `$type` — **директива-разрешение** (resolve-time): на input-стадии она замещается развёрнутой схемой типа из `types:`. После резолва дальше работает обычный input-DSL (валидация значений рекурсивна, как у любого inline-`object`/`array`).

3. **Резолв — service-level (MVP).** Имя `$type` ищется только в `types:` **того же сервиса**. **НЕ** local-per-scenario (типы не объявляются внутри `scenario/<name>/`), **НЕ** кросс-сервис (нельзя сослаться на тип другого сервиса). Это сознательная граница: один файл правды на сервис, без межсервисных зависимостей по схеме.

4. **Cycle-detection — обязателен.** Тип может ссылаться на тип (вложенность `$type` внутри `properties`/`items` объявленного типа). Резолвер обходит граф ссылок и **обязан** ловить цикл (`A → B → A`, в т.ч. самоссылку `A → A`) → ошибка `input_type_cycle`, не бесконечная развёртка. Граница глубины не ограничивается числом — ограничивается отсутствием цикла.

5. **DTO `/v1/scenarios` — backend-side resolve + `x-type`-аннотация.** При проекции схемы сценария в DTO эндпоинта каталога сценариев `$type` **резолвится backend-side ДО проекции**: клиент получает **уже развёрнутую** схему (UI строит форму по знакомому inline-формату, без знания про `types:`) плюс **forward-compat-аннотацию `x-type: <Имя>`** на узле, где стоял `$type`. UI её сегодня игнорирует; на вырост она позволяет специализированный виджет под именованный тип, не ломая текущих клиентов. `x-type` — чисто read-only DTO-аннотация, в самом YAML-источнике её не пишут.

6. **Замена `$ref`/`schemas/`.** Прежний нереализованный `$ref`-канал и папка `schemas/` **удаляются из канона**: упоминания в [architecture.md](../architecture.md) (дерево репо + блок «Опциональный `$ref`») и [service/manifest.md](../service/manifest.md) переписываются на `types.yml`/`$type`. Поскольку `$ref` никогда не был реализован, миграция не нужна — это замена задела, не breaking change.

**Формат — сводка.**

| Что | Где | Форма |
|---|---|---|
| Объявление типа | `service/<name>/types.yml` | `types: { <PascalCase>: <InputSchema> }` |
| Ссылка (объект) | любой `input:` сценария | `<param>: { $type: <Имя> }` |
| Ссылка (массив) | любой `input:` сценария | `<param>: { type: array, items: { $type: <Имя> } }` |
| Вложенность тип→тип | внутри схемы в `types:` | `$type` под `properties.<f>` / `items` объявленного типа (cycle-checked) |
| DTO-аннотация | ответ `/v1/scenarios` (read-only) | `x-type: <Имя>` на узле резолва |

**Резолв — порядок.** На input-стадии Keeper-а (та же фаза, что merge дефолтов / `required`-проверка): загрузить `types.yml` сервиса → для каждого `$type` в input-схемах сценария подставить развёрнутую схему типа (с cycle-detection) → дальше обычный input-резолв (merge → required → value-валидация) на уже развёрнутой схеме. `$type` нигде не доезжает до render-фазы — это структурная развёртка, не значение.

**Классы ошибок** (диагностика `soul-lint` и backend-резолва, область `input_type_*`, [naming-rules.md → Parser / validation errors](../naming-rules.md#error-codes)):

| Код | Когда |
|---|---|
| **`input_type_unknown`** | `$type: <Имя>` ссылается на тип, отсутствующий в `types:` сервиса. |
| **`input_type_cycle`** | Цикл в графе ссылок типов (`A→B→A`, самоссылка `A→A`). |
| **`input_type_duplicate`** | Дубль имени в секции `types:` (одно имя объявлено дважды). |
| **`input_type_ref_conflict`** | `$type` указан **вместе** с inline-схемой на том же узле (`type:`/`properties:`/`items:`/…) — ссылка и inline взаимоисключимы, узел либо `$type`, либо своя схема. |

**Границы MVP (что НЕ входит).**

- **`object` + `array-of-type` + вложенность тип→тип** — поддержаны. С обязательным cycle-detection.
- **Scalar-alias** (`types: { Port: {type: integer, min:1, max:65535} }` и `$type: Port` на скалярном поле) — **не входит** (можно добавить позже, форма та же, без breaking change; решение — отдельный propose-and-wait при реальном запросе).
- **Generics / параметризованные типы** — не входят.
- **Кросс-сервис** ссылки на типы другого сервиса — не входят (резолв строго service-level).
- **Local-per-scenario `types:`** (объявление типа внутри одного сценария) — не входит; типы только service-level.

**Импакт-потребители.**

- **`config.InputSchema` (`shared/config`)** — парсер input-DSL получает узловую директиву `$type` (поле-ссылка) + узловой `items: {$type}`; источник `types.yml` грузится service-level; резолвер с cycle-detection вызывается на input-стадии. Императивная value-валидация (рекурсия по развёрнутой схеме) не меняется.
- **`soul-lint validate-service`** — новые статпроверки `input_type_unknown` / `input_type_cycle` / `input_type_duplicate` / `input_type_ref_conflict` (см. [soul-lint.md](../soul-lint.md)). Резолв типов — часть проверки сценарного `input:`.
- **DTO `/v1/scenarios`** — backend разворачивает `$type` до проекции, добавляет `x-type`-аннотацию (forward-compat для UI-виджета).
- **`docs/input.md`** — новый раздел про `types:`-файл и `$type`-ссылку (формат, резолв, ошибки, границы MVP).
- **`architecture.md` / `service/manifest.md`** — замена нереализованного `$ref`/`schemas/` на `types.yml`/`$type`.

**Отвергнутые альтернативы.**
- **(а) Оставить `$ref` на внешний JSON-Schema-файл.** Отвергнуто: вторая схемная DSL рядом с нашим input-DSL (расхождение словаря/семантики/кодов ошибок), path-resolution-поверхность относительного `$ref`, синтаконфликт `$ref` под ключом `input:`. Named types живут **в том же DSL** — один словарь, один набор `input_*`-ошибок.
- **(б) Local-per-scenario типы.** Отвергнуто для MVP: типы переиспользуются **между** сценариями сервиса — их место service-level, иначе теряется сам смысл переиспользования. (Расширение возможно позже без breaking change.)
- **(в) Кросс-сервис ссылки.** Отвергнуто: вводит межсервисную зависимость по схеме (версионирование чужого `types.yml`, git-резолв) — несоразмерно цели wave 1. Резолв строго в пределах одного сервиса.

**Cross-ref.** [ADR-045](0045-param-dsl.md#adr-045-param-dsl-модулей--типизированные-input-поля-для-ui-формы-run-command) (сближение input-DSL модулей с `config.InputSchema` — named types применимы к тому же `InputSchema`); [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) (scenario `input:` — основной потребитель `$type`); [ADR-042](0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-в-ui--ui-не-хардкодит-динамические-каталоги) (backend разворачивает `$type` до проекции — UI получает знакомую inline-схему, не хардкодит про `types:`).
