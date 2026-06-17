# dockerfiles

L3c kind-cluster Dockerfile-ы для образов, грузящихся в kind через
`kind load docker-image` (без push в registry).

| Файл | Назначение | Slice |
|---|---|---|
| `keeper.Dockerfile` | `keeper:e2e-k8s` — distroless-runtime поверх `make build-linux` артефакта. Build-context — корень репо. | L3c-2 |

L3c-3 переиспользует [`tests/e2e-live/dockerfiles/debian-12.Dockerfile`](../../e2e-live/dockerfiles/debian-12.Dockerfile)
для Soul-pod (privileged systemd-PID-1, PM-decision: parity с L3b).

## Сборка

```sh
make docker-build-keeper        # = make build-linux + docker build -f .../keeper.Dockerfile -t keeper:e2e-k8s .
```

Harness (`Cluster.LoadDockerImage`) затем грузит образ в kind-cluster
(`kind load docker-image keeper:e2e-k8s --name <cluster>`), Deployment ссылается
через `imagePullPolicy: Never` — registry не нужен.

См. [tests/e2e-k8s/README.md](../README.md) → slice L3c-2/L3c-3.
