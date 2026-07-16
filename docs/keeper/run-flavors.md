# Forms of running work on hosts

Soul Stack provides several entry points for executing work on the fleet.
The choice depends on the semantics of the work and on access to the Soul agent.

## Decision table

| What I want | Endpoint | Transport | Mutates state | When |
|---|---|---|---|---|
| Apply a scenario to ONE incarnation | `POST /v1/incarnations/{name}/scenarios/{scenario}` (single-incarnation scenario-run, `incarnation.run`) | agent (mTLS EventStream) | yes | Stateful infra operation (deploy, configure, upgrade) |
| Same, but as a batch (several incarnations / 1000+ hosts) | `POST /v1/voyages` (`kind=scenario`) + `batch_size`/`concurrency` (batch = N incarnations, Leg) | agent | yes | Crowd control / canary / zonal rollout |
| Ad-hoc command on ONE Soul | `POST /v1/souls/{sid}/exec` | agent | no | Diagnostics of a single host, sync-30s response |
| Ad-hoc command on MANY Souls | `POST /v1/voyages` (`kind=command`, [ADR-043](../adr/0043-voyage.md)) | agent | no | `uptime` on a coven, fleet state check |
| Bare-host operation (agentless) | `POST /v1/push/apply` | ssh (via SshProvider) | no (synthetic scenario) | Bootstrap of new VMs, hosts without the soul daemon |

## Decomposition: "how" vs "what"

- **What:** scenario / module / synthetic scenario.
- **How:** via agent (pull) or via ssh (push).
- **Where:** target — coven / sids / where (CEL) / glob / regex.

These three dimensions are independent. The endpoint fixes the **what + how** combination.

## Cross-references

- Voyage - unified batch run (`kind=scenario` - batch of N incarnations by Legs; `kind=command` - multi-target ad-hoc): [ADR-043](../adr/0043-voyage.md). It absorbed Tide ([ADR-040](../adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) and ErrandRun ([ADR-041](../adr/0041-errandrun.md)) - both entities were removed at the implementation level (Wave 5, migrations 061/062), ADR-040/041 kept as superseded history.
- Push (Variant C): [ADR-032](../adr/0032-push-orchestrator.md).
- Errand single-SID: [ADR-033](../adr/0033-errand.md).
- Target resolution (choosing the target from the RBAC scope, without invocation-time AND-merge): [ADR-043 item 5](../adr/0043-voyage.md).
- CEL `matches()` / `glob()` in `target.where`: [docs/templating.md](../templating.md).
