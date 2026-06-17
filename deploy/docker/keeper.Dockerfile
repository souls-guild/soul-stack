# Dockerfile бинаря `keeper` (ADR-004). Multi-stage: builder на полном Go-тулчейне,
# runtime — distroless static-nonroot (только бинарь, без shell/libc/пакетного
# менеджера → минимальная поверхность атаки, «безопасность на первом месте»).
#
# Build-контекст — КОРЕНЬ моно-репо (там go.work + все 7 модулей). Собирать так:
#   docker build -f deploy/docker/keeper.Dockerfile -t soul-stack/keeper .
#
# Версия прокидывается ldflags-ом (см. Makefile, переменная VERSION) в
# main.version. Форма `-X main.<var>` обязательна: entrypoint — package main, и
# линкер молча игнорирует `-X` с полным import-path (бинарь остался бы
# 0.0.0-dev → `keeper version` врал бы версию).

# Go-версия синхронизирована с go.mod / go.work (go 1.26.3). При апгрейде Go —
# править здесь и в go.work одновременно.
FROM golang:1.26.3 AS builder

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

COPY --from=builder /out/keeper /usr/local/bin/keeper

# Конфиг ожидается смонтированным в /etc/keeper/keeper.yml (дефолтный путь keeper-бинаря).
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/keeper"]
CMD ["run"]
