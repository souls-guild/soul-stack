## What changed

<brief description of the PR: why, what exactly is changing, key files touched>

## Type of change

- [ ] Bug fix (non-breaking)
- [ ] Feature (non-breaking)
- [ ] Breaking change (contract / DB schema / RBAC / proto)
- [ ] Documentation only
- [ ] Refactor (no behavior change)

## Local checks

- [ ] `make check` green
- [ ] `make e2e` green (if apply-pipeline / keeper-side modules touched)
- [ ] `make test-race` green (if pubsub / lease / hot-path touched)

## Architecture

- [ ] Public contracts (OpenAPI / proto / RBAC / configs) not touched — skip this section.
- [ ] Touched — `docs/keeper/openapi.yaml` / `proto/*.proto` / `docs/keeper/rbac.md` updated.
- [ ] ADR affected — corresponding section of `docs/architecture.md` updated or a new ADR filed.
- [ ] New entities (names) — recorded in `docs/naming-rules.md`.

## Related ADR / documents

<links to sections of docs/architecture.md or other docs/>

## Other

<screenshots, command output, links to issues, notes for the reviewer>
