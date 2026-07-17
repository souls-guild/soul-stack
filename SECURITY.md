# Security Policy

Soul Stack is a configuration management system (Keeper + a fleet of Souls). A
compromised Keeper cluster means potential code execution across the entire managed
fleet, so we treat vulnerabilities with priority. Thank you for reporting responsibly.

## Supported versions

The project is a **closed small beta**. Only the current beta line is protected:

| Version           | Supported        |
|-------------------|------------------|
| `v0.1.0-beta.x`   | yes (current)    |
| anything older    | no               |

There is no stable release yet. Security fixes ship only in the next beta release;
we do not backport to earlier beta builds.

## How to report a vulnerability

**Do not open a public issue and do not describe the vulnerability in a pull
request.** The disclosure channel is this repository's private **GitHub Security
Advisory**:

1. Open the repository's **Security** tab → **Advisories** → **Report a vulnerability**
   (the "Report a vulnerability" button).
2. Fill in the advisory draft. The conversation there is visible only to you and the
   maintainers.

This keeps vulnerability details from leaking into the public sphere before a fix
ships.

### What to include

The more specific the report, the faster we can reproduce it:

- Affected component and version (output of `keeper version`).
- Type of vulnerability and surface (Operator API / EventStream Keeper<->Soul /
  plugin / web / CLI).
- Reproduction steps or a PoC — as minimal as possible.
- Impact assessment: what the attacker gains (RBAC bypass, RCE on the fleet, secret
  leakage, ...).
- Relevant log or audit-trail excerpt.

**Do not attach real secrets:** JWT tokens, Vault contents, SoulSeed private keys,
DSNs with passwords. Mask anything sensitive before submitting.

## Timeline expectations

The beta is **best-effort, no SLA**. We respond as capacity allows; there is no
guaranteed response or fix time during the closed-beta stage. For accepted advisories
we keep you updated on status through the same private thread.

## Threat model

The recorded assets, actors, surfaces, and residual risks of the cluster are in
[docs/security/threat-model.md](docs/security/threat-model.md). Any discrepancy
between that model and actual code behavior is considered a security bug (file it
via Security Advisory, not as a regular issue).
