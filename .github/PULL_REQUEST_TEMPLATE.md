## Что изменено

<краткое описание PR-а: зачем, что именно меняется, в каких файлах ключевая правка>

## Тип изменения

- [ ] Bug fix (non-breaking)
- [ ] Feature (non-breaking)
- [ ] Breaking change (контракт / схема БД / RBAC / proto)
- [ ] Documentation only
- [ ] Refactor (поведение не меняется)

## Локальные проверки

- [ ] `make check` зелёный
- [ ] `make e2e` зелёный (если задеты apply-pipeline / keeper-side modules)
- [ ] `make test-race` зелёный (если задеты pubsub / lease / hot-path)

## Архитектура

- [ ] Не задеты публичные контракты (OpenAPI / proto / RBAC / configs) — пропустить раздел.
- [ ] Задеты — `docs/keeper/openapi.yaml` / `proto/*.proto` / `docs/keeper/rbac.md` обновлены.
- [ ] Затронут ADR — соответствующий раздел `docs/architecture.md` обновлён или заведён новый ADR.
- [ ] Новые сущности (имена) — зафиксированы в `docs/naming-rules.md`.

## Связанные ADR / документы

<ссылки на разделы docs/architecture.md или другие docs/>

## Прочее

<скриншоты, output команд, ссылки на issues, заметки для ревьюера>
