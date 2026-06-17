# Dockerfile бинаря `soul-lint` (ADR-004) — офлайн-линтер, CLI-утилита.
# Multi-stage: builder на полном Go-тулчейне, runtime — distroless static-nonroot.
# Тонкий образ (один статический бинарь) — то, что и нужно для CLI в CI.
#
# Build-контекст — КОРЕНЬ моно-репо. Собирать так:
#   docker build -f deploy/docker/soul-lint.Dockerfile -t soul-stack/soul-lint .
#
# Типовое использование в CI — смонтировать репо и линтить конфиги:
#   docker run --rm -v "$PWD:/work" -w /work soul-stack/soul-lint validate-destiny destiny.yml

FROM golang:1.26.3 AS builder

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

# soul-lint version-переменной пока не имеет — ARG зарезервирован под будущую инъекцию.
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags '-s -w' \
        -o /out/soul-lint \
        ./soul-lint/cmd/soul-lint

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/soul-lint /usr/local/bin/soul-lint

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/soul-lint"]
