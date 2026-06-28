# Прод-образ бинаря `keeper` (ADR-004). Multi-stage: builder на полном
# Go-тулчейне, runtime — distroless static-nonroot (только бинарь, без
# shell/libc/пакетного менеджера → минимальная поверхность атаки, «безопасность
# на первом месте»).
#
# Образ самодостаточен: собирается из ОДНОГО `docker build` без предварительного
# `make build-linux` на хосте — builder-стадия пинит golang-тулчейн, поэтому
# сборка воспроизводима в любом окружении (CI / чужой registry) и не зависит от
# состояния `keeper/bin/` или версии локального Go. Готовый образ оператор
# перекладывает в свой registry и катает своим Helm-чартом (см. deploy/README.md).
#
# Build-контекст — КОРЕНЬ моно-репо (там go.work + все 7 модулей). Собрать с
# версионным тегом из git (так делает `make docker-keeper`):
#   docker build -f deploy/docker/keeper.Dockerfile \
#       --build-arg VERSION="$(git describe --tags --always --dirty)" \
#       -t soul-stack/keeper:<version> .
#
# Версия прокидывается ldflags-ом (--build-arg VERSION, по умолчанию 0.0.0-dev) в
# main.version. Форма `-X main.<var>` обязательна: entrypoint — package main, и
# линкер молча игнорирует `-X` с полным import-path (бинарь остался бы
# 0.0.0-dev → `keeper version` врал бы версию).
#
# Bootstrap первого Архонта (ADR-013) — ОТДЕЛЬНОЙ командой, НЕ в этом образе по
# умолчанию: `keeper init --archon=<aid>` (one-shot Job/exec), затем `keeper run`.
# CMD здесь — только `run`, БЕЗ `--initialize`: авто-bootstrap в проде опасен.

# Go-версия синхронизирована с go.mod / go.work (go 1.26.4). При апгрейде Go —
# править здесь и в go.work одновременно.
FROM golang:1.26.4 AS builder

WORKDIR /src

# Сначала только манифесты модулей — слой кешируется, пока не менялись deps.
# go.work связывает локальные модули, поэтому копируем все go.mod/go.sum дерева.
COPY go.work go.work.sum ./
COPY proto/go.mod proto/go.sum ./proto/
COPY proto/plugin/go.mod proto/plugin/go.sum ./proto/plugin/
COPY shared/go.mod shared/go.sum ./shared/
COPY sdk/go.mod sdk/go.sum ./sdk/
COPY keeper/go.mod keeper/go.sum ./keeper/
COPY soul/go.mod soul/go.sum ./soul/
COPY soul-lint/go.mod soul-lint/go.sum ./soul-lint/
RUN go mod download

# Остальные исходники.
COPY . .

# Статическая сборка без cgo — обязательна для distroless static (нет libc).
# Версия инжектится линкером в main.version (cmd/keeper/main.go).
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/keeper \
        ./keeper/cmd/keeper

# Runtime: distroless static с непривилегированным пользователем (uid 65532).
FROM gcr.io/distroless/static:nonroot

# OCI-метки — образ публикуется в registry оператора; org.opencontainers.* —
# стандартный канал происхождения/версии для сканеров и UI registry.
ARG VERSION=0.0.0-dev
LABEL org.opencontainers.image.title="soul-stack-keeper" \
      org.opencontainers.image.description="Soul Stack Keeper (ADR-004) — central node" \
      org.opencontainers.image.source="https://github.com/souls-guild/soul-stack" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=builder /out/keeper /usr/local/bin/keeper

# Конфиг ожидается смонтированным в /etc/keeper/keeper.yml — дефолтный путь
# keeper-бинаря (defaultConfigPath в cmd/keeper). Поэтому CMD не передаёт
# --config: оператор монтирует ConfigMap в /etc/keeper/keeper.yml, остальные
# секреты (PG-DSN, Redis, Vault, JWT signing key) keeper резолвит из Vault по
# *_ref из конфига (см. deploy/README.md → «Keeper в проде»).
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/keeper"]
# Прод-дефолт: только daemon, БЕЗ --initialize. Пустой реестр operators без
# --initialize → keeper run отказывается стартовать (ADR-013) — это barrier
# против тихого авто-bootstrap. Первый Архонт заводится отдельной командой
# `keeper init --archon=<aid>` (one-shot), см. шапку файла и deploy/README.md.
CMD ["run"]
