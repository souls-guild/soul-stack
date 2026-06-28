# deploy/ — упаковка и развёртывание Soul Stack

Build/ops-артефакты для запуска трёх бинарей (`keeper`, `soul`, `soul-lint`) в
контейнере и под systemd. Не domain-сущности — здесь нет бизнес-логики, только
обёртки сборки и запуска.

## docker/

Три multi-stage Dockerfile-а. Builder — `golang:1.26.3` (синхронизирован с
`go.work` / `go.mod`), runtime — `gcr.io/distroless/static:nonroot`: статический
бинарь без shell/libc/пакетного менеджера, непривилегированный пользователь
(uid 65532). Сборка статическая (`CGO_ENABLED=0`), `-trimpath`, `-ldflags "-s -w"`.

Build-контекст — **корень моно-репо** (там `go.work` + все модули). Примеры:

```sh
docker build -f deploy/docker/keeper.Dockerfile    -t soul-stack/keeper    --build-arg VERSION=$(git describe --tags --always --dirty) .
docker build -f deploy/docker/soul.Dockerfile      -t soul-stack/soul      --build-arg VERSION=$(git describe --tags --always --dirty) .
docker build -f deploy/docker/soul-lint.Dockerfile -t soul-stack/soul-lint .
```

`VERSION` инжектится ldflags-ом в форме `-X main.<var>` (entrypoint — package
`main`; форма с полным import-path линкером молча игнорируется). Версионируются
`soul` (`main.soulVersion`, печатается в Hello/BootstrapRequest для аудита),
`keeper` (`main.version`, печатает `keeper version`) и `soulctl`
(`main.soulctlVersion`, через `make build`). У `soul-lint` package-level
version-переменной ещё нет — `--build-arg VERSION` для него зарезервирован, в
`-X` не передаётся.

`.dockerignore` в корне репо отсекает `.git`, локальные `bin/`, `dev/`, `docs/`,
`.pm/` из контекста.

## Keeper в проде — образ + конфиг

Прод-сценарий: собрать образ `keeper`, переложить в **свой** registry, катать
**своим** Helm-чартом (k8s-манифесты Soul Stack не поставляет — нужен образ/бинарь).

### Собрать и опубликовать образ

`make docker-keeper` собирает прод-образ из `deploy/docker/keeper.Dockerfile`
(multi-stage, distroless-nonroot, версия в бинаре и OCI-метке) с тегом
`$(KEEPER_IMAGE):$(VERSION)`. `VERSION` — `git describe` (или release-override),
`KEEPER_IMAGE` — имя образа (по умолчанию `soul-stack/keeper`):

```sh
make docker-keeper                                   # soul-stack/keeper:<git-describe>
make docker-keeper VERSION=v0.2.0                    # фиксированный тег релиза
make docker-keeper KEEPER_IMAGE=registry.example.com/soul-stack/keeper VERSION=v0.2.0
```

Образ самодостаточен (тулчейн пинится в builder-стадии) — `make build-linux`
заранее не нужен. Перетегировать под свой registry и запушить:

```sh
docker tag  soul-stack/keeper:v0.2.0 registry.example.com/soul-stack/keeper:v0.2.0
docker push registry.example.com/soul-stack/keeper:v0.2.0
```

ENTRYPOINT — `keeper`, CMD — `run` (демон, **без** `--initialize`). Версионный
тег вместо `latest` — воспроизводимый откат и точный аудит в `keeper version`.

### Что keeper ждёт в проде

Образ несёт только бинарь. Всё остальное — оператор подаёт через mount/env;
пример полного конфига — `examples/keeper/keeper.yml`.

- **Конфиг** — `keeper.yml`, смонтированный в `/etc/keeper/keeper.yml`
  (дефолтный путь бинаря; CMD не передаёт `--config`). В k8s — ConfigMap →
  volumeMount. Несекретные настройки: `kid`, `listen`, `pool`, `otel`, `logging`,
  `reaper`, `acolytes`.
- **Vault — hard-required** (без него keeper не стартует): `vault.addr` + auth
  (approle `role_id` + `secret_id_file`, либо token). Из Vault keeper резолвит
  PKI (SoulSeed), JWT signing key, Essence-секреты, SSH CA, cloud-credential-ы.
  `secret_id` подавать через k8s Secret → файл, путь — в `vault.auth.secret_id_file`.
- **Postgres** — `postgres.dsn_ref` (vault-ref, напр. `vault:secret/keeper/postgres`,
  чтобы DSN с паролем не лежал в ConfigMap). Холодное хранилище кластера.
- **Redis** — `redis.addr` + `redis.password_ref` (vault-ref). Heartbeat/lease/
  pub-sub/лидер Reaper; для HA-кластера (`acolytes > 0`) обязателен.
- **JWT** — `auth.jwt.signing_key_ref` (vault-ref) + `issuer` + TTL. Без блока
  `auth.jwt` падает и `keeper init`, и `keeper run`.
- **Порты** (`listen`): gRPC bootstrap (`9442`, server-only TLS, отдельный
  listener), gRPC event_stream (`9443`, mTLS), OpenAPI (`8080`), MCP (`8081`),
  metrics (`9090`). TLS-материал (`server.crt/key`, `ca.crt`) — mount в
  `/etc/keeper/tls/` из Secret.

### Первый bootstrap (ОДИН раз на кластер)

`keeper run` при пустом реестре `operators` и **без** `--initialize` сознательно
отказывается стартовать (ADR-013) — защита от тихого авто-bootstrap. Первого
Архонта заводят отдельной командой с тем же образом и конфигом (one-shot
Job / `kubectl run` / `docker run --rm`):

```sh
keeper init --archon=archon-ops-01 --config /etc/keeper/keeper.yml
```

Команда под PG advisory-lock проверяет, что реестр пуст, создаёт первого Архонта
(`cluster-admin`, `permissions: ["*"]`), выпускает bootstrap-JWT (TTL из
`auth.jwt.ttl_bootstrap`) и пишет его файлом `mode 0400`. Дальше Deployment с
`keeper run` стартует штатно (реестр уже не пуст).

## systemd/

Юниты для `keeper` и `soul` (у `soul-lint` юнита нет — это CLI). Запускаются от
системного пользователя `soul-stack`, `Restart=on-failure`, `After=network-online`,
путь конфига вынесен в `EnvironmentFile` (`keeper.env` / `soul.env`).

Hardening различается по роли:

- **keeper** — жёсткий профиль (`ProtectSystem=strict`, `MemoryDenyWriteExecute`,
  `PrivateDevices` и т.д.): Keeper не меняет хост, ему изоляция не мешает.
- **soul** — мягкий профиль: Soul применяет Destiny (ставит пакеты, правит файлы,
  управляет сервисами), поэтому запись в систему НЕ запрещена. Не ужесточать без
  проверки apply-цикла.

Инструкции по установке (создание пользователя, каталоги, права) — в шапке
каждого `.service`-файла.

## nfpm/

Конфиги нативных пакетов deb/rpm — `keeper.yaml`, `soul.yaml`, `soul-lint.yaml`.
Каждый пакует собранный бинарь (`*/bin/<name>`) и, для демонов, systemd-юнит +
env-файл из `systemd/` + пример конфига из `examples/`. `soul-lint` — CLI, без
юнита и конфига, только бинарь.

Версия и архитектура подставляются из окружения (`${VERSION}` / `${ARCH}`),
которое прокидывает `make pkg`. Конфиг оператора (`/etc/keeper/keeper.env`,
`/etc/soul/soul.env`) помечен `config|noreplace` — upgrade его не перетирает.
Пример основного конфига кладётся отдельным именем (`keeper.yml.example` /
`soul.yml.example`), рабочий `*.yml` оператор создаёт сам.

## Packaging — как собрать

Все release-артефакты пишутся в `dist/` (в `.gitignore`, не коммитятся).
Таргеты аддитивны: в `make check` НЕ входят и его не ломают.

### SBOM (`make sbom`)

CycloneDX SBOM по трём релизным бинарям через `cyclonedx-gomod` в режиме `app`
(SBOM того, что реально слинковано в бинарь, по файлу на бинарь в `dist/sbom/`:
`keeper.cdx.json` / `soul.cdx.json` / `soul-lint.cdx.json`). Режим `app` (а не
`mod`) — потому что репо на `go.work`: `mod` под workspace для любого модуля
выдаёт SBOM корневого, а с `GOWORK=off` модули с локальными cross-module-зависимостями
не резолвятся. SBOM трёх бинарей покрывает library-модули (`proto`/`sdk`/`shared`)
транзитивно. Если инструмента нет в PATH — таргет печатает подсказку и выходит с
ошибкой (не молча):

```sh
go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest
make sbom
```

### deb/rpm (`make pkg`)

Нативные пакеты через `nfpm` (deb + rpm для каждого из трёх бинарей в
`dist/pkg/`). Бинари пересобираются под `linux/$(PKG_ARCH)` (deb/rpm — всегда
Linux, dev-машина может быть darwin), архитектура переопределяется
`make pkg PKG_ARCH=arm64`. Если `nfpm` нет в PATH — подсказка и выход с ошибкой:

```sh
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest
make pkg
```

Установка собранного пакета:

```sh
sudo dpkg -i dist/pkg/soul-stack-keeper_<version>_amd64.deb     # deb-дистрибутивы
sudo rpm  -i dist/pkg/soul-stack-keeper-<version>.x86_64.rpm    # rpm-дистрибутивы
# затем: cp /etc/keeper/keeper.yml.example /etc/keeper/keeper.yml — правки конфига
```

### Подпись образов (cosign) — post-publish, когда появится registry

Подпись образов и пакетов через cosign/sigstore **отложена**: реальная подпись
требует registry для публикации образов + keyless-identity через OIDC (или
приватного ключа подписи). Локальный репозиторий без CI/registry этого не имеет.

`make sign` — documented-stub: печатает причину отложенности и ссылку сюда,
завершается успешно (не блокирует пайплайн). Когда появятся CI + registry, план
такой:

- keyless-подпись образов в CI: `cosign sign <registry>/soul-stack/keeper:<tag>`
  под OIDC-identity workflow-а (Fulcio выдаёт эфемерный сертификат, Rekor
  логирует прозрачность);
- проверка на деплое: `cosign verify --certificate-identity=<workflow>
  --certificate-oidc-issuer=<issuer> <image>`;
- опционально — attach SBOM из `make sbom` к образу через `cosign attach sbom`.

## Отложено (следующий заход)

- **Подпись образов и пакетов** (cosign / sigstore) — см. раздел выше, ждёт
  CI + registry.
- **version-переменная** в `soul-lint` main-е (тогда ldflags `-X` и
  `--build-arg VERSION` начнут инжектить версию и в него — как уже сделано для
  `soul`/`keeper`/`soulctl`).
