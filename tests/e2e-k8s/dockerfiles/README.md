# dockerfiles

L3c kind-cluster Dockerfiles for images loaded into kind via
`kind load docker-image` (no registry push).

| File | Purpose | Slice |
|---|---|---|
| `keeper.Dockerfile` | `keeper:e2e-k8s` - distroless runtime on top of the `make build-linux` artifact. Build context is the repo root. | L3c-2 |

L3c-3 reuses [`tests/e2e-live/dockerfiles/debian-12.Dockerfile`](../../e2e-live/dockerfiles/debian-12.Dockerfile)
for the Soul pod (privileged systemd-PID-1, PM-decision: parity with L3b).

## Build

```sh
make docker-build-keeper        # = make build-linux + docker build -f .../keeper.Dockerfile -t keeper:e2e-k8s .
```

The harness (`Cluster.LoadDockerImage`) then loads the image into the kind cluster
(`kind load docker-image keeper:e2e-k8s --name <cluster>`), and the Deployment references
it via `imagePullPolicy: Never` - no registry needed.

See [tests/e2e-k8s/README.md](../README.md) -> slice L3c-2/L3c-3.
