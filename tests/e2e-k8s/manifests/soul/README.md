# manifests/soul — Soul StatefulSet for the L3c kind cluster (L3c-3)

Raw YAML manifests for the Soul StatefulSet and the associated systemd unit:

- `statefulset.yaml` — a `StatefulSet` with a privileged systemd-PID-1 (PM decision: parity
  with L3b `tests/e2e-live/dockerfiles/debian-12.Dockerfile`), replicas=1 for L3c-3.
  A headless `Service` (`clusterIP: None`) for a stable pod DNS
  `soul-0.soul.default.svc.cluster.local`.
- `soul.service` — the systemd unit for the soul agent, baked into the `soul:e2e-k8s`
  image via `tests/e2e-k8s/dockerfiles/soul.Dockerfile`. Runs `soul run
  --config /etc/soul/soul.yml` under systemd with `Restart=on-failure`. NOT enabled
  by default — the harness calls `systemctl start soul.service` after
  `soul init` (the CSR bootstrap flow populates SoulSeed at
  `/var/lib/soul-stack/seed`).

`StatefulSet`, not `Deployment` — a stable SID (FQDN) at the pod level is needed.
Privileged + systemd-PID-1 — for a real `core.service.*` via
systemctl and a real `core.pkg.*` via apt-install on the Debian-12 base.

Multi-Soul (replicas=N) — L3c-5.

See [tests/e2e-k8s/README.md](../../README.md) → slice L3c-3.
