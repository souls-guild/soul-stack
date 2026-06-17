# manifests/soul — Soul-StatefulSet для L3c kind-cluster (L3c-3)

Raw YAML manifest-ы Soul-StatefulSet и связанный systemd-unit:

- `statefulset.yaml` — `StatefulSet` privileged systemd-PID-1 (PM-decision: parity
  с L3b `tests/e2e-live/dockerfiles/debian-12.Dockerfile`), replicas=1 на L3c-3.
  Headless `Service` (`clusterIP: None`) для стабильного pod-DNS
  `soul-0.soul.default.svc.cluster.local`.
- `soul.service` — systemd-unit для soul-агента, baked в образ `soul:e2e-k8s`
  через `tests/e2e-k8s/dockerfiles/soul.Dockerfile`. Запускает `soul run
  --config /etc/soul/soul.yml` под systemd с `Restart=on-failure`. НЕ enabled
  by default — harness вызывает `systemctl start soul.service` после
  `soul init` (CSR Bootstrap-flow заполняет SoulSeed в
  `/var/lib/soul-stack/seed`).

`StatefulSet`, не `Deployment` — нужны стабильные SID (FQDN) на pod-уровне.
Privileged + systemd-PID-1 — для реального `core.service.*` через
systemctl и реального `core.pkg.*` через apt-install в Debian-12 base.

Multi-Soul (replicas=N) — L3c-5.

См. [tests/e2e-k8s/README.md](../../README.md) → slice L3c-3.
