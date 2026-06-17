# Формы запуска работы на хостах

Soul Stack даёт несколько entry-point-ов для исполнения работы на флоте.
Выбор зависит от семантики работы и доступа к Soul-агенту.

## Решающая таблица

| Что хочу | Endpoint | Транспорт | Mutates state | Когда |
|---|---|---|---|---|
| Применить scenario на ОДНУ incarnation | `POST /v1/incarnations/{name}/scenarios/{scenario}` (single-incarnation scenario-run, `incarnation.run`) | agent (mTLS EventStream) | да | Stateful infra-операция (deploy, configure, upgrade) |
| То же, но батчем (несколько инкарнаций / 1000+ хостов) | `POST /v1/voyages` (`kind=scenario`) + `batch_size`/`concurrency` (батч = N инкарнаций, Leg) | agent | да | Crowd control / canary / зональный rollout |
| Ad-hoc команда на ОДИН Soul | `POST /v1/souls/{sid}/exec` | agent | нет | Диагностика одного хоста, sync-30s ответ |
| Ad-hoc команда на МНОГО Souls | `POST /v1/voyages` (`kind=command`, [ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)) | agent | нет | `uptime` на coven, проверка состояния флота |
| Bare-host операция (без agent-а) | `POST /v1/push/apply` | ssh (через SshProvider) | нет (synthetic scenario) | Bootstrap новых VM, hosts без soul-демона |

## Декомпозиция «как» vs «что»

- **Что:** scenario / module / synthetic scenario.
- **Как:** через agent (pull) или через ssh (push).
- **Где:** target — coven / sids / where (CEL) / glob / regex.

Эти три измерения независимы. Endpoint фиксирует комбинацию **что + как**.

## Cross-references

- Voyage — унифицированный батчевый прогон (`kind=scenario` — батч N инкарнаций по Leg-ам; `kind=command` — multi-target ad-hoc): [ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон). Поглотил Tide ([ADR-040](../adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) и ErrandRun ([ADR-041](../adr/0041-errandrun.md#adr-041-errandrun--multi-target-обвязка-над-errand)) — обе сущности реализационно удалены (Wave 5, миграции 061/062), ADR-040/041 оставлены как superseded-история.
- Push (Variant C): [ADR-032](../adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-без-incarnationscenario).
- Errand single-SID: [ADR-033](../adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario).
- Target-резолв (выбор цели из RBAC-скоупа, без invocation-time AND-merge): [ADR-043 пункт 5](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон).
- CEL `matches()` / `glob()` в `target.where`: [docs/templating.md](../templating.md).
