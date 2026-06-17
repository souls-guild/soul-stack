# Keeper-image для L3c kind-cluster E2E. Single-stage: переиспользует артефакт
# `make build-linux` (статический keeper-linux-amd64 из `keeper/bin/`),
# COPY-им бинарь в distroless-runtime. PM-decision: `gcr.io/distroless/static`
# — минимальный attack-surface (без shell / без libc).
#
# Контекст сборки — корень репозитория (`docker build -f
# tests/e2e-k8s/dockerfiles/keeper.Dockerfile .`); ENTRYPOINT-аргументы и
# config-file берём из ConfigMap, mount-ить который будет K8s-Deployment.
#
# Образ ОДНОРАЗОВЫЙ — собирается локально, грузится в kind через `kind load
# docker-image`, в registry не публикуется (см. Makefile::docker-build-keeper).

FROM gcr.io/distroless/static:nonroot

# nonroot UID/GID (65532). distroless по умолчанию выставляет USER=nonroot,
# но дублируем явно — keeper.yml ожидает писать в каталоги, владельцем
# которых должен быть тот же UID.
USER nonroot:nonroot

COPY keeper/bin/keeper-linux-amd64 /keeper

ENTRYPOINT ["/keeper"]
CMD ["run", "--config", "/etc/keeper/keeper.yml", "--initialize"]
