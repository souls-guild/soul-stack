# ADR-042. Backend-driven dynamic data в UI — UI не хардкодит динамические каталоги.

**Контекст.** Companion-UI [`soul-stack-web`](https://github.com/co-cy/soul-stack-web) ([ADR-035](0035-distribution-split.md#adr-035-distribution-split--core-apicli-vs-web-ui)) потреблял часть динамических каталогов хардкодом — например, список RBAC-permissions был зашит в TS. Это породило класс багов: backend завёл/переименовал permission (`soul.read`), UI о нём не знал → запрос с `unknown_permission`, рассинхрон UI ↔ backend-каталог. Module-каталог при этом уже отдавался эндпоинтом (`GET /v1/modules`) — паттерн непоследователен. Нужен общий принцип: что в UI можно хардкодить, а что обязан отдавать backend.

**Решение** (вариант A2, propose-and-wait пройден с architect 2026-05-29).

1. **UI НЕ хардкодит динамические каталоги.** Любой каталог, который backend централизованно валидирует или который расширяется без релиза UI, отдаётся через каталог-эндпоинт OpenAPI; UI фетчит его в рантайме.

2. **Backend отдаёт идентификаторы + машинные метаданные**, не human-текст: `resource`/`action` для permission, `selector_keys`, enum-значения, `required`/`secret`-флаги и т.п. Human-label и перевод — на стороне UI (i18n) с **graceful fallback на идентификатор**: нет лейбла → показывается сам идентификатор, UI не падает.

3. **Граница** (ядро ADR):
   - **Backend-driven (UI обязан фетчить):** RBAC permission-каталог, module-catalog, enum-ы статусов прогонов / incarnation / errand-run / tide, ключи и типы селекторов таргетинга, и любой closed-каталог, который **(а)** централизованно валидирует backend или **(б)** расширяется без релиза UI.
   - **Допустимо в UI:** вёрстка / layout / иконки / цветовые токены, i18n-строки и human-labels, локальные пользовательские предпочтения. **Критерий:** «не влияет на принимаемость запроса backend-ом и не растёт backend-side».

**Последствия.**
- Новый permission / модуль / статус в backend виден в UI **без релиза UI** — как минимум как идентификатор.
- Рассинхрон UI-хардкода с backend-каталогом устранён как класс багов.
- Цена — UI делает дополнительный fetch каталогов (кэшируемо).

**Инстансы принципа.**
- `GET /v1/modules` — module-catalog (уже есть).
- `GET /v1/permissions` — RBAC permission-каталог (вводится этим ADR).
- `GET /v1/event-types` — event-type-каталог для Tiding-подписки (источник [`herald/eventtypes.go`](https://github.com/co-cy/soul-stack/blob/main/keeper/internal/herald/eventtypes.go), ADR-052).

Сквозное требование к компоненту web зафиксировано в [requirements.md](../requirements.md); OpenAPI остаётся единственным контрактом core ↔ web ([ADR-035](0035-distribution-split.md#adr-035-distribution-split--core-apicli-vs-web-ui)).
