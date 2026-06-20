# ADR-035. Distribution split — core (API+CLI) vs web (UI).

> **Статус: amended.** [ADR-055](0055-embed-ui-bundle.md) активирует отложенный embed-compat-shim (см. §«Что отложено» → «Embed compat-shim для air-gapped enterprise») как опциональный **default-ON** embed UI на маршруте `/ui` для беты (single-binary onboarding). Это **не разворот** разделения дистрибуции: companion-source-of-truth (п.2) и toolchain-split (инварианты: core CI не зависит от web-build, нет npm в Makefile/go.work) сохранены; разворачивается только инвариант п.3/«Отвергнутые (б)» «никакого embed UI-assets в keeper» — теперь допускается завендоренный **собранный артефакт** (не исходники). Детали — [ADR-055](0055-embed-ui-bundle.md).

**Контекст.** На MVP-стадии в репо возник `ui/` — React+TS+Vite scaffold UI (5 страниц + 7 тестов). По мере роста стало ясно: TS-tooling (node_modules, vite, vitest) живёт по другим правилам, чем Go-ядро; UI — опциональный компонент (operator работает через CLI/MCP/OpenAPI), но тянет за собой ~400+ npm-пакетов в основной репо.

**Решение.**
1. **Core distribution = только Go-артефакты.** В этом репо живут: `keeper`, `soul`, `soul-lint`, `soulctl`, `proto/`, `sdk/`, `shared/`, `docs/`, `examples/`. `ui/` удаляется.
2. **Web — отдельный репо `soul-stack-web`.** Companion repository по модели **SaltStack ↔ salt-manager**, **OpenStack ↔ Horizon**, **Kubernetes ↔ Dashboard**.
3. **Контракт между core и web — OpenAPI.** [`docs/keeper/openapi.yaml`](../keeper/openapi.yaml) — единственный публичный API-контракт. Web потребляет его через `openapi-typescript`-генерацию типов; никаких других точек интеграции (никакого embed UI-assets в keeper-бинарь, никакого общего SDK).
4. **Релизные циклы независимы.** Core и web версионируются отдельно (web может выпускать минорные релизы под одну major-версию core API). UI compat-matrix фиксируется в `soul-stack-web/README.md`.
5. **Operator MAY работать без UI.** `soulctl` + MCP + прямой OpenAPI — полнофункциональный интерфейс оператора. UI — usability-layer, не обязательная зависимость.
6. **`docs/web/README.md` в core-репо** — заглушка-pointer на внешний репо.

**Инварианты.**
- Никаких HTML/CSS/TS-файлов в этом репо (кроме `docs/`-fixtures).
- Никаких npm-зависимостей в `Makefile`/`go.work`.
- Core CI/CD не зависит от web-build.
- Web потребляет OpenAPI как **read-only** контракт (только generated types, никаких custom-расширений).

**Отвергнутые альтернативы.**
- (а) UI в подкаталоге `ui/` основного репо — отвергнуто (раздутый repo, смешанная toolchain, разные релизные циклы).
- (б) UI как embedded assets в keeper-бинарь (как pgAdmin/Grafana) — отвергнуто (keeper-бинарь должен оставаться small, без UI-зависимостей; deployment-model — keeper отдаёт API, UI деплоится отдельно).
- (в) UI как monorepo с git-submodule — отвергнуто (submodules неудобны на практике; OpenAPI-vendor через простой `cp` или артефакт-publishing проще).

**Что отложено.**
- **OpenAPI как published artifact.** Сейчас web-команда тянет openapi.yaml через `cp`/clone core-репо. В будущем — published artifact (например, в Harbor или GitHub Releases) с semver-меткой. Не блокирует MVP.
- **OIDC SSO в web.** Сейчас JWT paste-форма (UI scaffold нашёл этот пробел). OIDC — отдельный slice в web-репо после ADR-amendment к ADR-014.
- **Embed compat-shim для air-gapped enterprise.** Если enterprise-вариант захочет single-binary с UI — отдельная сборка `keeper-bundled` (отдельный билд-таргет), не дефолт. Не входит в open-core MVP.
