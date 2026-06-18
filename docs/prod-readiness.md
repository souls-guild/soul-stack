# Prod-readiness — GA-gap роадмап

Что **не готово** для продакшена / GA. Бета `v0.1.0-beta.1` выпущена и feature-complete; этот документ — список разрывов между бетой и GA, по результатам по-коду аудита GA-готовности (2026-06-17).

Двойной источник правды по границам:

- [known-limitations.md](known-limitations.md) — что НЕ входит в **бету** (граница со стороны пользователя беты).
- **этот файл** — что нужно закрыть до **GA** (граница со стороны прод-готовности).

> **Не путать с [roadmap.md](roadmap.md).** `roadmap.md` дрейфует относительно фактического кода и не является источником правды по GA-границам — он будет актуализирован отдельно. При расхождении верить known-limitations.md + этому файлу (оба сверены с кодом).

## GA-scope решения (зафиксированы 2026-06-17)

Эти три решения задают рамку: что входит в GA, а что остаётся пост-GA. Они меняют приоритеты пунктов ниже.

| Решение | Значение | Следствие для роадмапа |
|---|---|---|
| **Топология кластера** | Эластичный (autoscaling) | Балансировка scale-out (**Shepherd**) поднимается в P0: без неё autoscale бессмыслен (новый инстанс простаивает). |
| **Cloud-provisioning** | **Пост-GA** (не в GA-scope) | Cloud-CRUD остаётся явным known-limitation, не блокер GA. |
| **Целевой флот** | **Тысячи** хостов | Audit-партиционирование и готовый мониторинг — P1 (тысячи → ощутимый PG INSERT-rate); полный 100k-ramp — P2. |

---

## P0 — prod-блокеры GA

Без закрытия этих семи пунктов GA выпускать нельзя.

### 1. e2e-live (L3b): доказать зелёным + сделать blocking

Nightly-job `e2e-live` гоняет **реальный `soul`-бинарь в privileged-контейнере** (полный apply/Scry-pipeline), но стоит с `continue-on-error: true` ([.github/workflows/nightly.yml](../.github/workflows/nightly.yml)). Реальный apply может быть сломан незаметно — job не блокирует. Нужно: добиться стабильно зелёного прогона и снять `continue-on-error`, сделав его blocking-гейтом.

### 2. Clean-room getting-started (DoD-1)

Живой человек поднимает кластер «с нуля» на **чистой машине** строго по [getting-started.md](getting-started.md) / [operations/deb-onboarding.md](operations/deb-onboarding.md), без подсказок из головы автора. Цель — поймать скрытые предусловия и разрывы в онбординг-доке. Оценка: ~1 человеко-день.

### 3. Release-дистрибуция (supply-chain)

Релиз сейчас — **ручные `make`-таргеты**; автоматизированного `release.yml` в `.github/workflows/` нет (есть только `ci.yml` / `nightly.yml`).

- registry-образы не публикуются;
- `make sign` — **заглушка** (печатает причину, `exit 0` — [Makefile](../Makefile), [known-limitations.md → Supply-chain](known-limitations.md#supply-chain-подпись-образов-отложена)): реальной cosign/sigstore-подписи нет.

Нужен воспроизводимый релиз-pipeline: сборка артефактов → публикация образов в registry → реальная cosign-подпись.

### 4. Shepherd — балансировка нагрузки при scale-out

Не реализован (**0 кода**, греп по `shepherd` пуст; описан как PLANNED в [operations/scaling.md → Shepherd](operations/scaling.md#shepherd--балансировка-нагрузки-при-scale-out)). При **эластичном кластере** (GA-scope) это P0: новый инстанс после scale-out простаивает до естественного churn-а стримов — autoscale без активного rebalance бессмыслен.

### 5. recovery-lease (`reclaim_apply_runs`) live под крашем инстанса

Правило `reclaim_apply_runs` (Reaper подбирает зависшие после краша Keeper-инстанса прогоны) реализовано ([keeper/internal/reaper/](../keeper/internal/reaper/voyage_reclaim.go)), но **disabled-by-default**, и его поведение под реальным крашем инстанса **не доказано live** (runbook есть — [operations/recovery-reclaim-apply-runs.md](operations/recovery-reclaim-apply-runs.md), [known-limitations.md → Recovery](known-limitations.md#recovery-прерванных-прогонов--выключено-по-умолчанию)). На multi-keeper GA-кластере правило **обязательно ON**: иначе при краше инстанса прогоны зависают в `applying`. Нужно: live-валидация под убийством инстанса + перевод в ON для multi-keeper.

### 6. Внешний pentest + identity-пробелы

- Независимый **внешний pentest** не проводился ([known-limitations.md → pentest](known-limitations.md#внешний-pentest--не-проводился-внутренний-gate-достаточен-для-беты)); для GA — обязателен.
- **Нет немедленного отзыва JWT** до `exp`: после `revoke` Архонта его токены живут до истечения, аварийный отзыв — только ротацией signing-key ([known-limitations.md → Identity](known-limitations.md#identity-оператора-только-jwt)).
- **mTLS-cert identity оператора** — пост-MVP ([ADR-014](adr/0014-operator-identity.md)); для GA закрыть либо немедленный revoke, либо machine-identity.

### 7. Снять `continue-on-error: true` с трёх классов проверок в CI

Сейчас informational (не блокируют merge) — структурный риск, тихий регресс не остановит PR:

| Job | Файл | Что не блокирует |
|---|---|---|
| `integration` (testcontainers) | [ci.yml](../.github/workflows/ci.yml) | Интеграционные тесты |
| `govulncheck` | [ci.yml](../.github/workflows/ci.yml) | Сканер уязвимостей Go-модулей |
| `e2e-live` | [nightly.yml](../.github/workflows/nightly.yml) | Реальный apply/Scry (см. P0-1) |

Для GA эти три класса должны быть blocking (с предварительной стабилизацией flaky — см. P1).

---

## P1 — hardening (до GA, после P0)

- **Audit-партиционирование** `audit_log` по `created_at` (declarative partitioning / BRIN — [ADR-022](adr/0022-audit-pipeline.md)). Флот «тысячи» → ощутимый PG INSERT-rate (нагрузка показала ≈2 INSERT/хост на Voyage — [load-testing.md §8.3](testing/load-testing.md#83-ось-c--voyage-по-флоту)).
- **Готовые Grafana-дашборды + Prometheus-алерты** — в репо отсутствуют; метрики/OTel публикуются ([observability.md](observability.md)), но out-of-box наблюдаемости нет.
- **MCP-полнота** — Cadence и Audit-read без MCP-tool-ов; `keeper.soul.list` / `keeper.push.cleanup` — `not_implemented`-stub ([known-limitations.md → MCP](known-limitations.md#mcp-не-покрывает-все-домены)).
- **Multi-keeper нагруз-прогон shard-буфера `applybus`** — фикс maxclients-cliff (sharded-каналы) не проверен на cross-keeper-пути под нагрузкой.
- **Стабилизация flaky integration-тестов** (≈6 в `keeper/internal/api`, order-dependent / auth-race) — сейчас не карантинены; нужны до того, как `integration` станет blocking (P0-7).
- **Voyage presence-резолв → Redis-lease** вместо PG `souls.status` (инвариант «горячее → Redis», не синхронный PG-write на горячем пути).
- **Push: Teleport `proxy_jump`** — не доделан (узкий профиль push — [known-limitations.md → Push](known-limitations.md#push-agentless-по-ssh--узкий-профиль)).
- **SoulBeacon live-loop e2e** + UI `/oracle/fires` — backend `GET /v1/oracle/fires` не реализован, страница-заглушка ([known-limitations.md → /oracle/fires](known-limitations.md#ui-oraclefires--заглушка)).
- **DR**: отработать `restore` на staging + CLI-команды `keeper --check-config` / `conclave-evict` / `issue-token` (в `soulctl` сейчас нет — [soulctl/](../soulctl/README.md)).
- **Cloud-CRUD → явный known-limitation** (пост-GA): 6 CloudDriver-плагинов есть, но нет REST `/v1/providers` — операционально недоступен ([known-limitations.md → Cloud-provisioning](known-limitations.md#cloud-provisioning--не-в-бете)).
- **Coverage-report** в CI (видимость покрытия; жёсткий gate — P2).

---

## P2 — nice-to-have (можно пост-GA)

- **100k-нагруз-прогон** (Ф2: распределённый harness) — для целевых «тысяч» 25k стримов покрыты с запасом расчётом ([load-testing.md §6](testing/load-testing.md), Ф2 — бэклог).
- **Conclave-метрики** (`keeper_conclave_*`).
- **Reproducible builds** (`SOURCE_DATE_EPOCH`).
- **Hard coverage-gate** (порог покрытия блокирует merge).
- **Soul auto-upgrade флота** — уточнить механизм самообновления `soul`-агентов.

---

## Сильные места (готово, не стабы)

Чтобы роадмап не читался как «ничего не готово» — это уже работает реально, не заглушки:

- **Сквозные возможности** — метрики, OTel, hot-reload + writeback конфига, ротация логов, Vault-интеграция, RBAC, OpenAPI — реализованы и работают ([requirements.md](requirements.md), [observability.md](observability.md)).
- **Ядро** — pull-режим (агент-демон `soul`), scenario-DSL, Voyage (батч по флоту), Scry (probe), RBAC — готовы и доказаны на живом стенде.
- **SBOM** (CycloneDX, `make sbom` — [Makefile](../Makefile)) — готов (в отличие от подписи).
- **Module-path rename** на `github.com/souls-guild/soul-stack` — выполнен.

---

## Доказанная нагрузка

Источник правды — [testing/load-testing.md](testing/load-testing.md) (Ф0 + срез Ф1 **измерены** на живом стенде 2026-06-17; полный 100k-ramp — расчётный, Ф2).

- **Keeper линеен по стримам** до измеренного N=1000 коннектов; per-soul прирост RSS ≈ **0.12–0.15 MiB/душу** (приростная дельта, не абсолют — [§8.1](testing/load-testing.md#8-измеренные-результаты-ф0--срез-ф1-2026-06-17)).
- **Экстраполяция на 100k по дельте** ≈ 15–19 GiB RSS → **3–4 инстанса** на 100k по модели (в бюджете [scaling.md → Sizing](operations/scaling.md#sizing-infrastructure-под-100k-vm-приблизительно)); реальный cliff на N=1000/300 **не достигнут** — точный per-soul под 100k остаётся задачей Ф2.
- **Read-API** держит с запасом (3811 req/s, p99 5.9 ms на `GET /v1/souls`).
- **applybus maxclients-fix** (sharded-каналы) устранил cliff на ~10k command-Voyage; cross-keeper-проверка под нагрузкой — P1.

---

## См. также

- [known-limitations.md](known-limitations.md) — границы беты (со стороны пользователя).
- [testing/load-testing.md](testing/load-testing.md) — измеренная нагрузка + план Ф2.
- [operations/scaling.md](operations/scaling.md) — sizing, узкие места, Shepherd / Conclave / Acolyte.
- [security/threat-model.md](security/threat-model.md) — статус внутреннего security-gate и внешнего pentest.
- [RELEASING.md](../RELEASING.md) — процедура релиза (docs-currency gate, теги).
