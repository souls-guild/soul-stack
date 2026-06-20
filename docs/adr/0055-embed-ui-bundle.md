# ADR-055. Embed UI bundle — опциональный single-binary keeper с UI на `/ui`

> **Статус: active.** Решение пользователя (имена + default-ON-тоггл) + дизайн architect-а. **Amends [ADR-035](0035-distribution-split.md) п.3** — активирует ранее отложенный embed-compat-shim (ADR-035 «Что отложено» → embed для air-gapped/enterprise) уже для беты как **default-ON**, НЕ разворот разделения дистрибуции. Канон фиксируется docs-first ДО кода; имплементация — отдельный слайс (см. §Слайс-карта).

**Контекст.** [ADR-035](0035-distribution-split.md) разделил дистрибуцию: core = только Go-артефакты, web — companion-репо `soul-stack-web`, контракт core↔web = OpenAPI, UI деплоится отдельно. Для беты этот двухкомпонентный onboarding — лишний барьер: оператор, поднявший один `keeper`-бинарь, хочет сразу увидеть UI, без отдельной сборки/деплоя/раздачи статики soul-stack-web. ADR-035 уже предусмотрел этот случай в «Что отложено» («Embed compat-shim для air-gapped enterprise … single-binary с UI») — настоящий ADR активирует его раньше и делает **дефолтом** (не отдельной сборкой `keeper-bundled`), под bootstrap-беты.

Прецедент embed статики в keeper уже есть: OpenAPI-вьювер `GET /docs` несёт go:embed-бандл RapiDoc (~840 КБ) в `keeper/internal/api/docsassets/` ([ADR-054](0054-openapi-code-first.md), Amendment 2026-06-15, механизм A). Embed собранного web-UI — то же по механике, шире по объёму.

**Решение.** Keeper опционально (**default-ON**) встраивает завендоренный build-снапшот UI (`soul-stack-web`) через `go:embed` и раздаёт его на маршруте `/ui`.

- **(а) Пакет `keeper/internal/webui/`, ассеты в `keeper/internal/webui/assets/`.** Собранный статический артефакт UI лежит в подпапке `assets/` (НЕ `dist/`). Папка `dist/` молча съедается gitignore-правилом `dist/` (релизные артефакты) — embed получил бы пустое дерево, бандл бы не попал в бинарь и не отловился ревью. `assets/` нейтрально к gitignore.

- **(б) Маршрут `/ui` + `/ui/*` с SPA-fallback.** `GET /ui` и `GET /ui/*path` отдают статику из embed-дерева; неизвестный под-путь (deep-link SPA-роутер вроде `/ui/incarnations/42`) → fallback на `index.html` (стандартный single-page-application паттерн), а НЕ 404. Существующие реальные ассеты (`/ui/assets/*.js`, `*.css`) отдаются как файлы. Mount — рядом с `/docs`/`/healthz`/`/openapi.yaml`, **вне `/v1`**.

- **(в) Статика ПУБЛИЧНА — parity с `/docs`.** `/ui`-дерево раздаётся без auth (JS/CSS/HTML — не секрет; та же модель, что публичный shell `/docs`). Защищён **API**, не статика: `/v1/*` остаётся за `RequireJWT` + RBAC + default-deny ([ADR-014](0014-operator-identity.md), [ADR-028](0028-rbac-storage.md), [ADR-047](0047-purview.md)). UI после загрузки фетчит `/v1/*` с Bearer-JWT, который оператор вводит в самом UI (JWT paste-форма, [ADR-035](0035-distribution-split.md) п. «Что отложено» → OIDC SSO остаётся отдельным slice). Раздача статики API-поверхность не раскрывает — данные приходят только из `/v1` за JWT.

- **(г) Config-тоггл `web_ui_enabled` (скаляр `*bool`).** Корневой config-ключ keeper, `*bool`: `nil` (опущен) → **default-ON** (бета хочет UI из коробки); явный `false` → opt-out (статика не монтируется). Симметрия footgun-guard-у соседних подсистем (`tempo.enabled`/Conductor/Toll — default-ON при наличии бэкенда), но без зависимости от инфраструктуры: UI вшит в бинарь, внешнего бэкенда не требует. **Restart-required** (не hot-reload): эффективное значение читается один раз на старте и запекается в смонтированный роутер; SIGHUP / API-reload его не переключает — `/ui`-mount на лету не появляется и не снимается. Симметрично restart-required-тогглам `toll.enabled` / `tempo.enabled` ([ADR-021](0021-hot-reload-config.md): инфраструктура поднимается/гасится только на старте). Atomic re-mount роутера для routing-тоггла отвергнут как over-engineering: статика вшита в бинарь, состояния не несёт, переключение редкое (onboarding-беты), а disposal/swap chi-роутера под трафиком — лишний риск ради бинарного флага.

- **(д) Делит listener `:8080` — новых портов/systemd-юнитов НЕТ.** `/ui` монтируется в тот же chi-роутер и тот же HTTP-listener Operator API, что `/v1`/`/docs`/`/healthz`. Никакого отдельного web-сервера, порта, systemd-сервиса или reverse-proxy. Single-binary в прямом смысле: один процесс, один порт.

- **(е) Прирост бинаря ~+2–5 МБ.** Собранный SPA-бандл (минифицированные JS/CSS/HTML) — порядка единиц мегабайт; приемлемо для single-binary onboarding (тот же порядок, что RapiDoc-бандл `/docs` × несколько). Не тянет node_modules/vite/исходники — только готовый артефакт.

**Кросс-репо: source-of-truth и синк.**

- **Source-of-truth UI = companion-репо `soul-stack-web`** ([ADR-035](0035-distribution-split.md) п.2). Исходники (React/TS, vite-конфиг, тесты) живут ТАМ и в core-репо НЕ копируются. В core попадает **только завендоренный build-снапшот** (вывод `vite build`) в `keeper/internal/webui/assets/`.

- **Синк — `scripts/sync-webui.sh` + drift-guard `make check-webui`** — калька механизма plugin-template ([sync-template.sh](../../scripts/sync-template.sh) / `check-template` в [Makefile](../../Makefile)). `sync-webui.sh` зеркалит build-вывод из companion (`../soul-stack-web/<build-output>` → `keeper/internal/webui/assets/`, `rsync --delete` / rm+cp fallback). `check-webui` — CI-guard на расхождение «забыли синк после пересборки UI в companion»; **skip без companion** (на чужой машине/CI companion-репо рядом может не быть — гейт не падает, а пропускается с warning, как `check-template`). Точное имя build-output-папки companion и dev-story (как локально собрать/обновить снапшот) — слайс sync-механизма.

- **vite `base: '/ui/'`.** Чтобы относительные пути ассетов SPA резолвились под префиксом `/ui` (а не от корня) — companion-сборка настраивается с `base: '/ui/'`. Это правка в `soul-stack-web` (отмечено для frontend-слайса), не в core.

**Уточнение инварианта [ADR-035](0035-distribution-split.md) «никаких HTML/CSS/TS в репо».** Инвариант сохраняется в части **исходников**: React/TS-исходники, vite-конфиг, npm-зависимости в core-репо по-прежнему запрещены (toolchain-разделение ADR-035 не разворачивается — `go.work`/`Makefile` не тянут node, core CI не зависит от web-build). Допускается ровно **собранный статический артефакт** (минифицированный JS/CSS/HTML-бандл) в `keeper/internal/webui/assets/` — ровно как уже допущен RapiDoc-бандл `/docs` ([ADR-054](0054-openapi-code-first.md)). Граница: **исходник UI — запрещён, собранный артефакт — допущен.**

**Безопасность.**
- API-граница не ослаблена: `/v1/*` — JWT + RBAC + default-deny без изменений. Публична только статика (parity `/docs`).
- Air-gapped/офлайн: UI вшит, CDN не тянется (та же мотивация, что go:embed RapiDoc) — single-binary несёт всё.
- Прирост attack-surface — статик-файл-сервер на публичном пути; SPA-fallback отдаёт только embed-дерево (никаких path-traversal во внешнюю FS — раздача из `embed.FS`, не из disk-FS).

**Слайс-карта** (по дизайну architect, по правилу массовых операций — пилот перед тиражом механизма):
1. **ADR** (этот документ) — канон docs-first.
2. **Пилот backend** — пакет `keeper/internal/webui/` со **стаб-embed** (минимальный заглушечный `index.html`), маршрут `/ui` + `/ui/*` SPA-fallback, тоггл `web_ui_enabled`, guard-тесты (mount/SPA-fallback/тоггл-off → не смонтировано/публичность статики). Доказывает механику до реального бандла.
3. **Frontend** — `vite base: '/ui/'` в companion `soul-stack-web` (зона frontend-агента).
4. **Sync-механизм** — `scripts/sync-webui.sh` + `make check-webui` (+ запись в `check`-цепочку), dev-story.
5. **Реальный снапшот** — первый завендоренный build-снапшот UI в `assets/` + проверка end-to-end (UI грузится на `/ui`, фетчит `/v1` с JWT).
6. **Docs** — пользовательская дока onboarding-беты (зона docs-writer).

**Связь с ADR.**
- **[ADR-035](0035-distribution-split.md)** — **amends** (статус 0035 → amended): активирует отложенный embed-compat-shim как default-ON для беты; toolchain-split и companion-source-of-truth ADR-035 сохранены, разворачивается только п.3-инвариант «никакого embed UI-assets» (теперь embed допущен для собранного артефакта).
- **[ADR-054](0054-openapi-code-first.md)** — прецедент go:embed-статики в keeper (`/docs` RapiDoc-бандл, публичный mount вне `/v1`); `/ui` следует той же модели.
- **[ADR-021](0021-hot-reload-config.md)** — config-ключ `web_ui_enabled` присутствует в конфиг-контракте, но **restart-required** (не hot-reloadable): mount читается один раз на старте, как `toll.enabled` / `tempo.enabled`.
- **[ADR-014](0014-operator-identity.md)** — API за JWT; UI фетчит `/v1` с Bearer, статика публична.

**Отвергнутые альтернативы.**
- **(а) Build-from-companion в core-CI** (core-CI клонирует soul-stack-web и собирает UI). Отвергнут: cross-repo build-зависимость + node-toolchain в core-CI — прямое нарушение toolchain-split ([ADR-035](0035-distribution-split.md): «core CI/CD не зависит от web-build», «никаких npm-зависимостей в Makefile/go.work»). Завендоренный build-снапшот + drift-guard держит core-CI go-only.
- **(б) git submodule на soul-stack-web.** Отвергнут вторично — уже отвергнут в [ADR-035](0035-distribution-split.md) (отвергнутая альтернатива «(в) UI как monorepo с git-submodule»: submodules неудобны на практике). Sync-снапшот проще.
- **(в) Скачивание dist из GitHub Release при `go build`/старте.** Отвергнут: сеть в `go build` (ломает офлайн/air-gapped сборку), токены доступа к приватному релизу, недетерминированный билд. Прямое противоречие мотиву single-binary-офлайн.
