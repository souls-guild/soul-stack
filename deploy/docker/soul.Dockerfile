# Dockerfile бинаря `soul` (ADR-004) — демон-агент. Multi-stage: builder на
# полном Go-тулчейне, runtime — distroless static-nonroot (только бинарь).
#
# Build-контекст — КОРЕНЬ моно-репо. Собирать так:
#   docker build -f deploy/docker/soul.Dockerfile -t soul-stack/soul .
#
# Версия инжектится ldflags-ом в main.soulVersion (см. Makefile). Форма `-X
# main.<var>` обязательна: entrypoint — package main, и линкер молча игнорирует
# `-X` с полным import-path (бинарь остался бы 0.0.0-dev → неверная версия в
# Hello/BootstrapRequest → искажённый аудит).

FROM golang:1.26.4 AS builder

WORKDIR /src

COPY go.work go.work.sum ./
COPY proto/go.mod proto/go.sum ./proto/
COPY proto/plugin/go.mod proto/plugin/go.sum ./proto/plugin/
COPY shared/go.mod shared/go.sum ./shared/
COPY sdk/go.mod sdk/go.sum ./sdk/
COPY keeper/go.mod keeper/go.sum ./keeper/
COPY soul/go.mod soul/go.sum ./soul/
COPY soul-lint/go.mod soul-lint/go.sum ./soul-lint/
RUN go mod download

COPY . .

# soulVersion печатается в Hello/BootstrapRequest для аудита — инжектим версию.
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.soulVersion=${VERSION}" \
        -o /out/soul \
        ./soul/cmd/soul

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/soul /usr/local/bin/soul

# Конфиг ожидается в /etc/soul/soul.yml (дефолтный путь soul-бинаря).
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/soul"]
CMD ["run"]
