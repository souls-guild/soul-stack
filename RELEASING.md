# Выпуск версии Soul Stack

Процедура выпуска релиза. Действует для беты (`vX.Y.Z-beta.N`) и далее.

Инвариант версионирования: **один git-тег на корень репозитория = одна логическая версия всех 7 модулей** go.work ([ADR-011](docs/adr/0011-go-layout.md)). Отдельных версий у `keeper`/`soul`/`soul-lint`/`soulctl`/`shared`/`sdk`/`proto` нет. Версия инъектится в бинари на этапе линковки через `-X main.<var>` (см. `Makefile`: `KEEPER_LDFLAGS`/`SOUL_LDFLAGS`/`SOULCTL_LDFLAGS`), бинарь печатает её командой `keeper version` / `soul version` / `soulctl version`. Это не противоречит ADR-007 (версия артефактов Service/Destiny/Module — git ref): здесь версионируется сам продукт-сборка, не пользовательские артефакты.

## Процедура

### (a) Freeze HEAD

Зафиксировать релизный коммит на `main`. С этого момента в релиз идёт только то, что уже в дереве; новые фичи — после тега.

### (b) Зелёный гейт

Прогнать полный гейт на Linux-CI:

```sh
make check              # fmt + vet + build + test + drift-проверки + vuln + lint examples
make test-integration   # testcontainers (нужен docker)
make e2e                # L3a fast-loop (нужен docker)
```

Здесь — docker-зависимые уровни до L3a. Длительный L3b (`make e2e-live`) — отдельный **блокирующий** pre-tag шаг (e), L3c (`make e2e-k8s`) гоняется on-demand. Релиз не выпускается, пока гейт не зелёный.

### (c) Bump CHANGELOG

В [CHANGELOG.md](CHANGELOG.md) (формат Keep a Changelog) перенести накопленное из `[Unreleased]` в новую версионную секцию `[vX.Y.Z-beta.N]`, проставить дату (или пометку «дата фиксируется при теге» — по стилю файла), отдельным блоком перечислить known-limitations релиза. `[Unreleased]` после этого остаётся пустым (для пост-релизного задела). Изменение CHANGELOG входит в релизный коммит до тега.

### (d) Сверка актуальности документации (docs-currency gate)

**Обязательный шаг до создания тега.** `docs-writer` проводит аудит
актуальности документации — drift код↔дока по всем документируемым
поверхностям (API/OpenAPI, CLI `soulctl`, поведение core-модулей и per-module
README, конфиг-схемы, поведение proto-контракта Keeper↔Soul). Каждое
расхождение либо закрывается правкой доки, либо явно фиксируется (known-limitation
в CHANGELOG / флаг `adr_drift` PM-у, если разошлись код и ADR). Релиз не
тегируется, пока в документируемых поверхностях остаётся незакрытый или
незафиксированный drift.

### (e) e2e-live gate (real apply на реальном хосте) — блокирующий

**Обязательный шаг до создания тега.** unit/integration гоняют стабы; единственный
тест, доказывающий что `apply` работает на **реальном** хосте end-to-end (реальный
soul-бинарь в privileged Debian-контейнере, реальный `apt`-install + systemd), —
L3b `make e2e-live` (кейсы nginx / drift / redis-cluster). Без зелёного e2e-live тег
**не режется**: apply на реальном хосте мог сломаться, и поймает это только этот
уровень. Это локальный эквивалент CI-gate — без расхода GitHub-минут.

1. Docker-free гейт — зелёный:

   ```sh
   make check    # build + vet + test + check-gen/openapi/template/doc-links + vuln + lint
   ```

2. L3b real apply — **все три кейса** зелёные:

   ```sh
   make e2e-live    # nginx / drift / redis-cluster — real apt-install + systemd
   ```

   На **WSL2 + Docker-Desktop** перед прогоном пробросить реальный WSL2-хост-IP
   (контейнер-соул не достучится до keeper через `host.docker.internal` — тот
   указывает на DD-VM-шлюз, не на WSL2-хост):

   ```sh
   E2E_KEEPER_HOST=$(hostname -I | awk '{print $1}') make e2e-live
   ```

   На native-Linux env-override не нужен (CI-дефолт `host.docker.internal`).
   Детали окружения и рецепт — [tests/e2e-live/README.md](tests/e2e-live/README.md).

Релиз не тегируется, пока `make check` и все кейсы `make e2e-live` не зелёные.

### (f) Аннотированный git-тег

Один тег на корень репозитория:

```sh
git tag -a vX.Y.Z-beta.N -m "Soul Stack vX.Y.Z-beta.N"
git push origin vX.Y.Z-beta.N
```

Первый бета-тег — `v0.1.0-beta.1`. Тег **аннотированный** (не lightweight): `git describe` берёт ближайший аннотированный тег, и именно он попадает в `VERSION` при сборке.

### (g) Сборка артефактов на теге

С checked-out тега (чтобы `git describe` дал чистую версию без `-dirty`/хеша) собрать релизные артефакты:

```sh
make pkg    # нативные пакеты deb + rpm (nfpm) → dist/pkg/, бинари под linux/amd64
make sbom   # CycloneDX SBOM по keeper/soul/soul-lint → dist/sbom/
```

`make pkg` пересобирает бинари под `linux/$(PKG_ARCH)` (default `amd64`; `make pkg PKG_ARCH=arm64` — для arm) с теми же ldflags-инъекциями версии. `make sbom` строит SBOM в режиме `app` (граф того, что реально слинковано). Оба таргета требуют внешний tooling (`nfpm`, `cyclonedx-gomod`) — в `make check` не входят, ставятся через `go install` (подсказка печатается, если не найден). Для голой кросс-сборки бинарей без пакетов — `make build-linux`.

### (h) Раздача

Приложить артефакты из `dist/pkg/` и `dist/sbom/` к GitHub Release соответствующего тега (или раздать тестерам беты напрямую — на закрытой бете дистрибуция также build-from-source, см. [CONTRIBUTING.md](CONTRIBUTING.md)).

## Отложено до GA (пост-бета)

- **Подпись артефактов (cosign / sigstore).** `make sign` — documented-stub: реальная подпись требует registry для публикации образов + keyless-identity через OIDC (или приватный ключ). План и команды — раздел «Подпись образов (cosign)» в [deploy/README.md](deploy/README.md).
- **Registry-образы.** Публикация контейнерных образов `keeper`/`soul` в registry — после GA; на бете образы собираются локально только для E2E (`make docker-build-keeper` / `make docker-build-soul`, грузятся в kind, в registry не публикуются).
