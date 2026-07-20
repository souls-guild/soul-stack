# Soul Stack Licensing

A human-readable explanation of the license. The [`LICENSE`](LICENSE) file text
is the legally binding one; this document explains its practical meaning.

> **Draft edition.** Wording is being finalized with legal counsel.
> Full decision and rationale — [ADR-016](docs/adr/0016-parity-license.md).

Soul Stack is **fair-code**: the source is open and available, but commercial
provisioning of the product to third parties as a service is restricted.

## Summary

| Component | License | Meaning |
|---|---|---|
| **Core** — Keeper, Soul, soulctl, soul-lint, built-in `core.*` modules (this repository) | **BSL 1.1** (fair-code) → after 2 years each version becomes **Apache 2.0** | code is open; production use is broadly permitted, except reselling as a hosted/managed service |
| **Web interface** (`soul-stack-web`) | **BSL 1.1** → Apache 2.0 | same as the core |
| **SDK, examples, plugins** (`sdk/*`, `examples/`, official/community `soul-mod-*`) | **Apache 2.0** | fully free, including proprietary third-party plugins |
| **Premium packs, enterprise modules** (later) | commercial | separate products on top of the open core |

## What BSL is and why not "fully open source"

**BSL (Business Source License) 1.1** is the same model used by MariaDB, Sentry,
CockroachDB, HashiCorp, and n8n. The source is open: it can be read, built,
modified, and used in production — with one restriction: you may not
provide Soul Stack to third parties as a hosted/managed service. After
**2 years**, each specific version automatically becomes **Apache 2.0** —
fully open software. It is a sliding window: today's version becomes
Apache in two years, the next one two years after its own release.

Why fair-code, and neither full closed-source nor pure Apache:

- **Trust requires openness.** Soul Stack runs an agent and works with
  secrets on your servers — the source must be available for audit.
- **Project sustainability.** An open core without resale protection easily
  turns into someone else's paid service without contributing back. BSL closes
  exactly this scenario while leaving everything else open.
- **Return to the commons.** The Change Date guarantees that every version
  eventually becomes Apache 2.0 — the restriction is temporary, not permanent.

## What you can do (Additional Use Grant)

**Permitted without a separate license:**

- **Internal use** — managing your own or corporate infrastructure,
  including commercial operations.
- **Development, testing, evaluation, demos.**
- **Professional and managed services for your clients** — you operate
  Soul Stack and hand the client the **result**, while Soul Stack itself
  as a product is not provided to the client.

**Requires a separate commercial license:**

- **Hosted / managed service for third parties** — providing Soul Stack
  (or a modification of it) to third parties so that they get access to it
  and use it as a service or product (including for free).
- **White-label** — offering Soul Stack under a different brand.
- **Embedding / OEM** — embedding Soul Stack into a third-party product (not
  covered by the starting grant).

### The line for managed providers (MSP)

1. **Soul Stack on the client's servers, the client owns it.** You configure and
   maintain it — this is the client's internal use. **Allowed.**
2. **Soul Stack on your side, the client sees only the result of the work.**
   The client does not log into Soul Stack and does not use it as a product.
   **Allowed** (managed services above).
3. **Soul Stack on your side, the client logs in and uses it.** This is
   reselling Soul Stack as a service. **Requires a commercial license.**

## Why SDK and plugins are Apache 2.0

The ecosystem needs to grow without friction. Plugins are separate processes that
communicate with the core over gRPC and are not, legally, a derivative work of the core.
That's why **SDK, examples, and plugins are under Apache 2.0**: authors are free to
publish their modules under any license, including proprietary paid plugins. The BSL
restriction applies only to reselling the core itself as a service, not to writing
extensions for it.

## Brand and "official"

The name, logo, and "official" / "certified" / "official managed" statuses are protected
by **trademark**, not by the code license. Permitted: self-hosting, training,
plugin development, mentioning compatibility. Not permitted: calling a fork
"Soul Stack," selling "certification" or "official managed" status on our behalf.

## Contributions (CLA)

The Contributor License Agreement is set up before the first external contributor —
under fair-code it is needed to preserve the right to the Additional Use Grant, the Change
License, and future license amendments. Details — [CONTRIBUTING.md](CONTRIBUTING.md).

## FAQ

**Can I use Soul Stack in a commercial company for free?**
Yes — for managing your own infrastructure, including commercial
operations. The restriction only applies to reselling Soul Stack to third
parties as a service.

**I'm an MSP — can I service clients on Soul Stack?**
Yes, if the client receives the result of your work, not access to Soul Stack as
a product. If the client logs into and uses Soul Stack directly — you need a
commercial license (see the MSP line above).

**What exactly becomes Apache 2.0, and when?**
Each specific version of the core and web UI, 2 years after its own
first public release. The restriction lifts automatically, retroactively for that
version.

**Does my plugin have to be open?**
No. SDK and plugins are Apache 2.0; you publish your own plugin under any
license, including proprietary and paid ones.

---

Full decision and rationale — [ADR-016](docs/adr/0016-parity-license.md).
Legal text — [`LICENSE`](LICENSE).
