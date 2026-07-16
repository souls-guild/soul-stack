# pilot-destiny — пилот `apply:destiny` (слайс A) + `include:` (слайс B)

Минимальный сервис, демонстрирующий **`apply:destiny`** — делегирование работы
сценария в переиспользуемую destiny с изолированным render-проходом (V2, ADR-009)
— и **`include:`** (раскрытие соседних файлов в плоский список до render).

## Что показывает

- **scenario-include:** `scenario/create/main.yml` подключает соседний
  `marker.yml` через `include:` — он раскрывается ДО render (двухуровневый резолв
  scenario-локально → service-level, [orchestration.md §6](../../../docs/scenario/orchestration.md#6-two-level-resource-resolution)).
  Внутри `marker.yml` — задача `apply: { destiny: pilot-flat, input: {…} }`.
- **apply:destiny:** apply-задача раскрывается в задачи destiny (вклеиваются в
  общий план со сквозными индексами).
- **within-destiny include:** `pilot-flat/tasks/main.yml` подключает
  `record.yml` через `include:` — within-destiny include раскрывается при загрузке
  destiny ([destiny/tasks.md §4](../../../docs/destiny/tasks.md#4-basic-blocks)).
  Итоговый плоский список — `core.file.present` (0) + `core.exec.run` (1).
- **Изоляция:** destiny видит ТОЛЬКО свой `input:` (`marker_file`/`marker_payload`),
  переданный через `apply.input`. scenario-scope (`input.path`/`input.content`,
  vars, register, soulprint) в destiny-env НЕ попадает — структурная граница.
- **Добор defaults:** `marker_mode` не передаётся в `apply.input` → добирается из
  `default` контракта destiny (`0644`).

## Раскладка destiny

В этой фикстуре destiny `pilot-flat` лежит рядом с сервисом
(`pilot-flat/`) — герметичный L0-прогон грузит её из локального дерева,
git не нужен. В проде git-URL destiny выводится из
`keeper.yml::default_destiny_source` + `{name}`, а `ref` — из `service.yml →
destiny[]` (ADR-007).

## Прогон L0

```sh
cd keeper
go run ./cmd/soul-trial run ../examples/service/pilot-destiny/scenario/create/tests/render-flat/case.yml
```

Ожидаемо: `PASS` — две задачи destiny с резолвнутым input.
