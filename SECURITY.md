# Security Policy

Soul Stack is a configuration-management system (a Keeper cluster plus a population of
Souls). A compromised Keeper cluster can mean code execution across every managed
Soul, so we treat vulnerabilities as high priority. Thank you for reporting
responsibly.

## Supported versions

The project is in **public beta**. Only the current beta line receives security fixes:

| Version           | Supported        |
|-------------------|------------------|
| `v0.1.0-beta.x`   | yes (current)    |
| anything older    | no               |

There is no stable release yet. Security fixes ship in the next beta release; we do
not backport to earlier beta builds.

## How to report a vulnerability

**Do not open a public issue, and do not describe the vulnerability in a pull
request.** Use a private channel:

1. **GitHub private advisory (preferred).** In the repository, go to the **Security**
   tab → **Advisories** → **Report a vulnerability**. The thread is visible only to you
   and the maintainers, and it keeps everything in one place.
2. **Email.** If you can't use GitHub advisories, write to **security@soul-stack.com**.
   If you want to send encrypted details, say so in a first plaintext message and we'll
   arrange a key.

Either way, please don't disclose the issue publicly until a fix has shipped.

### What to include

The more specific the report, the faster we can reproduce it:

- Affected component and version (output of `keeper version`).
- Type of vulnerability and the surface (Operator API / EventStream Keeper↔Soul /
  plugin / web / CLI).
- Reproduction steps or a PoC — as minimal as possible.
- Impact assessment: what the attacker gains (RBAC bypass, RCE across Souls, secret
  leakage, …).
- A relevant log or audit-trail excerpt.

**Do not attach real secrets** — JWT tokens, Vault contents, SoulSeed private keys, or
DSNs with passwords. Mask anything sensitive before submitting.

## Timeline expectations

Beta support is **best-effort, no SLA**. We respond as capacity allows; there is no
guaranteed response or fix time during the beta. For accepted advisories we keep you
updated through the same private thread, and we'll credit you in the advisory once a
fix is out (unless you prefer to stay anonymous).

## Threat model

The recorded assets, actors, surfaces, and residual risks of the cluster are in
[docs/security/threat-model.md](docs/security/threat-model.md). Any discrepancy between
that model and actual code behavior is treated as a security bug — report it through a
private advisory, not as a regular issue.
