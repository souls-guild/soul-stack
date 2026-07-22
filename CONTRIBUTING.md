# Contributing to Soul Stack

Thanks for wanting to help. Soul Stack is in **public beta**, and both **issues**
and **pull requests** are open — from humans and from AI coding assistants alike
(see the [AI assistants](README.md#ai-assistants) note in the README).

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to contribute

- **Report a bug** — open an issue with the "Bug report" template. Include the output
  of `keeper version`, your environment, reproduction steps, and expected vs. actual
  behavior. A minimal reproducible artifact (a Destiny/scenario file) speeds things up
  a lot.
- **Request a feature** — open an issue with the "Feature request" template and
  describe the problem you're trying to solve, not only the solution you have in mind.
- **Send a pull request** — fixes, docs, tests, and new modules are all welcome. For
  anything larger than a small fix, please open an issue first so we can agree on the
  approach before you spend time on it.

Security vulnerabilities are the one exception: **do not** file them as issues or PRs.
Report them privately — see [SECURITY.md](SECURITY.md).

## Contributor License Agreement (CLA)

Before your first pull request can be merged, you'll be asked to sign the
**[Contributor License Agreement](CLA.md)** — once, and it then covers all your future
contributions.

It's automated: a bot comments on your first PR, and you sign by replying to that
comment. The CLA is a **license-back** agreement — you keep the copyright to your
contribution and grant the project a license to use and relicense it. This is what
lets the project honor the Business Source License (the Additional Use Grant, the
Change License, and future license changes) on behalf of everyone. See
[CLA.md](CLA.md) for the full text and the rationale.

## Development setup

Soul Stack is a Go workspace (`go.work`) of several modules. To build and check
locally:

```sh
git clone https://github.com/souls-guild/soul-stack.git
cd soul-stack
make build     # keeper / soul / soul-lint / soulctl → <module>/bin/
make check     # the full local gate — run this before you push
```

`make check` is the gate. It reproduces what CI runs — formatting, `go vet`, build,
unit + plugin tests, code generation drift, OpenAPI/template checks, the embedded web
UI check, internal doc-link integrity, the vulnerability scan, and the linter. **A PR
is expected to have `make check` green.** CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml))
runs the same target plus the E2E and integration suites.

Some heavier targets are opt-in and worth running when relevant:

- `make e2e` — fast-loop end-to-end (Postgres + Redis + Vault via testcontainers).
  Run it if you touched the apply pipeline or keeper-side modules.
- `make test-race` — run if you touched pub/sub, leases, or any hot path.

To reproduce a bug on a live stack, bring up the local dev circuit (Postgres + Redis +
Vault via docker-compose) with `make dev-up` / `make dev-stand`; details in
[docs/dev/local-setup.md](docs/dev/local-setup.md). The operator quickstart is
[docs/getting-started.md](docs/getting-started.md).

Build toolchain: Go (version pinned in `go.work`), `make`, and — only for `make gen` —
`protoc` with the `protoc-gen-go` / `protoc-gen-go-grpc` plugins. Committed code builds
without `protoc`.

## Conventions

- **Language: English.** All source — code, comments, log/error strings, tests,
  godoc, `examples/`, and docs — is in English. (Product prose on the documentation
  site is translated separately.)
- **Names come from the dictionary.** Use the Soul Stack vocabulary
  ([docs/naming-rules.md](docs/naming-rules.md)) — Keeper, Souls, Destiny, Soulprint,
  Essence. A brand-new name or concept is proposed and
  agreed before it lands (see the ADR process).
- **Design goes through ADRs.** Architectural decisions live in
  [docs/adr/](docs/adr/README.md) (one file per ADR) with an overview in
  [docs/architecture.md](docs/architecture.md). A change that alters a contract
  (OpenAPI / proto / RBAC / config schema) or a design decision should update the
  relevant doc or ADR — the PR template has a checklist for this.
- **Keep comments lean.** Explain the non-obvious *why*, not the obvious *what*.
- **Tests for behavior.** New behavior comes with a regression test; prefer
  end-to-end over integration over unit where it makes sense.

## Before you open a pull request

- `make check` is green.
- Commits are focused, with messages that say *why*.
- Docs/ADRs updated if you changed a documented surface.
- The PR description explains the change and how you verified it. Fill in the
  [pull request template](.github/PULL_REQUEST_TEMPLATE.md).

## Before you open an issue

- Check [docs/known-limitations.md](docs/known-limitations.md) — some behavior there
  is a deliberate out-of-scope, not a bug.
- Confirm it reproduces on a fresh build from the current `main`.
