# ADR-080. Coven and Trait — one label world: inheritance by membership, union at read

- **Status.** Active. Supersedes the R1 relocation amendment of
  [ADR-060](0060-traits.md) (Trait per-soul → per-incarnation with a materialized
  projection) and extends [ADR-008](0008-coven-stable-tags.md) with the same rule
  for the Coven axis. Landed by NIM-121.

- **Context.** An operator label — a Coven tag or a Trait key-value pair — can be
  meaningfully attached at two levels: to one **host** (this VM is Bobik's) and to
  a whole **incarnation** (this Redis instance belongs to the DBA team). Both are
  real and both are wanted; neither replaces the other.

  Until now the system could not hold both. Each axis kept the label in exactly
  one place and faked the other level by **physically copying the label
  downwards**:

  - **Trait.** [ADR-060](0060-traits.md) R1 made `incarnation.traits` the sole
    source of truth and projected it in a MATERIALIZED way into member hosts'
    `souls.traits` (`SyncTraitsToHosts`, a full `BulkReplaceTraits` wired into
    incarnation-create and host-bind). The per-soul write path
    (`POST /v1/souls/traits`) was left deprecated and, by construction,
    **overwritten by the projection on the next sync** — an operator could not put
    a label on a host and have it survive.
  - **Coven.** Before [NIM-124](0008-coven-stable-tags.md) the incarnation's
    identity reached its hosts because `incarnation.name` was injected into
    `souls.coven[]`. NIM-124 correctly split membership out into
    `incarnation_membership` and stripped the synthetic value from the column —
    which removed the copy and, with it, the only path by which an
    incarnation-level tag reached its hosts.

  The result today is that **an incarnation-level label grants nothing on its
  hosts.** Host visibility resolves over the `souls` table alone —
  `soulpurview.Columns` maps `coven`/`sid`/`traits` onto columns of `souls` and
  `SelectAll` joins nothing — so a role scoped `coven=redis-prod` or
  `trait.owner=dba` sees the incarnation but not the VMs inside it. The Trait
  projection papered over exactly this hole for one of the two axes, at the cost
  of destroying host-local labels.

  Copying is also wrong on its own terms. `incarnation_membership` is
  deliberately **M:N** ([migration 099](../../keeper/migrations/099_create_incarnation_membership.up.sql)),
  and the projection replaces the whole `souls.traits` map per incarnation — so
  for a host in incarnations `A` and `B`, syncing `A` erases what `B` projected.
  No lock ordering fixes that: the projection and the per-soul write target the
  same column with a wholesale REPLACE.

- **Decision (fixed by the user, propose-and-wait closed).** **A label lives only
  where it was attached, and is never copied.** What consumers see is the
  **union**, computed at read time.

  1. **Storage is two independent axes per label kind — no projection.**

     | Column | Holds | Written by |
     |---|---|---|
     | `souls.coven[]` / `souls.traits` | labels of THIS host | the operator, directly |
     | `incarnation.covens[]` / `incarnation.traits` | labels of THIS incarnation | the operator, directly |

     Nothing syncs one into the other. `SyncTraitsToHosts` and both of its
     hookpoints (incarnation-create, `core.soul.registered` bind) are **removed**,
     not disabled. This makes "the projection silently overwrote my host label"
     structurally impossible rather than merely unlikely.

  2. **Effective labels of a host = its own ∪ every incarnation it belongs to.**
     Membership is `incarnation_membership` (NIM-124), so a host in two
     incarnations inherits from both. The incarnation's **name** is part of what
     it contributes to the Coven axis, mirroring the incarnation-side resolver
     which already treats the name as a coven tag (`covens && $scope OR name =
     ANY($scope)`). This does **not** revert NIM-124: the name is not written into
     `souls.coven[]`, it participates only in the union at read time.

  3. **A key collision is a UNION, not a contest.** If the incarnation carries
     `owner=dba` and the host carries `owner=bobik`, the host's effective
     `owner` is **`[dba, bobik]`** — both, with no precedence rule. Access is
     granted by ANY match, so a role scoped `trait.owner=dba` and a role scoped
     `trait.owner=bobik` both reach that host. This falls out of the existing data
     shape rather than stretching it: a Trait value is already scalar **or a list**
     ([ADR-060](0060-traits.md) item 1), and Coven is already a set.

     A precedence rule ("host wins" / "incarnation wins") was rejected: it makes
     one of two deliberately-attached labels invisible, and whichever way it is
     pointed it silently revokes access somebody was granted.

  4. **Every consumer of labels reads the union, and they all read the same one.**

     - **RBAC visibility** — the `coven` and `trait` dimensions of a souls-scope
       predicate become "the host's own OR inherited", rendered as a correlated
       `EXISTS` over `incarnation_membership ⋈ incarnation`. The pushdown stays in
       SQL (offset/total stay exact, no Go post-filter); the single-object Go gate
       (`soulpurview.InScope`) is fed the same union.
     - **Targeting** — `soulprint.self.covens` / `.traits` and
       `soulprint.hosts[].*` project the union, so a `where:` predicate and a scope
       predicate never disagree about the same host.
     - **Reactor subject binding** ([ADR-030](0030-vigil-oracle.md), added by
       NIM-224) — the `coven` half of a Vigil's and a Decree's subject resolves
       over the union too, so `coven: [<incarnation>]` binds a rule to that
       incarnation's members. This is the reading that used to come free from the
       injected name and was lost with it; both halves of the chain read it, since
       a Vigil that never ships emits no Portent for the Decree to match.
     - **Push provider routing** ([ADR-032](0032-push-orchestrator.md) Level 2,
       added by NIM-251) — `push.coven_default_providers` is matched against the
       union, so labelling an incarnation puts all of its hosts behind one
       bastion. Routing is the one consumer that cannot take "both": it selects
       exactly ONE provider, so it keeps a tiebreak (own tags before inherited,
       each group alphabetical). That is an ordering of the LOOKUP, not a
       precedence between labels — it is chosen so inheritance is purely
       additive and no already-routed host silently changes auth perimeter.

     Keeping these in step is the point: a union applied to some readers and not
     others is a new class of bug — one that shows up as a rule matching nothing,
     with no error anywhere.

     **Membership is not a label question, and must never be answered from the
     union.** `incName ∈ effectiveCovens` holds both for a member and for a host
     merely carrying a host-attached tag spelled like the incarnation's name, so
     any gate deciding *belonging* — the Oracle's cross-incarnation guard, the
     Choir voice invariant, the roster — reads `incarnation_membership` directly.
     The union widens what a rule may **see**; only the relation says where a host
     **belongs**. Conflating them turns this ADR's widening into an escalation
     path.

  5. **The per-soul write path is first-class again, and gated like Coven.**
     `POST /v1/souls/traits` (permission `soul.traits-assign`, MCP
     `keeper.soul.traits-assign`) loses its deprecated marking; `merge` / `replace`
     / `remove` already exist and are unchanged. It gains **gate (b)** — the pair
     being written must lie inside the operator's own trait-scope — exactly as
     `soul.coven-assign` refuses a label outside the operator's coven-scope
     (`AssignCovenTyped`). Gate (a) (target hosts ⊆ the operator's coven-scope) is
     untouched.

     Gate (b) is now **required**, and its previous absence was reasoned from a
     premise this ADR removes. [ADR-060](0060-traits.md) argued that a trait key is
     "not an RBAC scope dimension" and so needs no key gate. That stopped being
     true when NIM-128 made `trait.<key>=v` a scope dimension, and this ADR makes
     a host-attached trait grant visibility permanently rather than until the next
     sync. Without gate (b) any holder of `soul.traits-assign` could hand a host to
     a foreign role, or hide it from an auditing one, by stamping or clearing a
     pair.

  6. **Permissions and audit keep their names.** No new permission is introduced —
     `soul.traits-assign` / `incarnation.traits-set` and the audit events
     `soul.traits-changed` / `incarnation.traits_changed` (KEYS only, never
     values) carry over unchanged. Renaming them would break every deployed role
     config for a semantic change that does not alter who may call what.

  7. **Migration
     ([106](../../keeper/migrations/106_prune_projected_soul_traits.up.sql)) prunes
     the projected residue.** On the day the projection is removed, whatever it
     last wrote is sitting in `souls.traits` and would silently become
     host-attached — a duplicate that no longer disappears when the label is
     removed from the incarnation. The migration deletes from each host's
     `souls.traits` every key whose value equals what one of its incarnations
     carries for that key, leaving genuinely host-local pairs alone. Effective
     labels are unchanged by the prune (the pruned pairs come straight back
     through inheritance) — only their storage location is corrected.

- **Consequences.**

  - **Visibility widens for existing roles.** A role scoped `coven=<X>` or
    `trait.<k>=<v>` now also sees the hosts of matching incarnations, where before
    it saw the incarnation alone. This is the defect being fixed, not a
    side effect — but it is a real change to what a deployed role can read, and it
    lands with the release rather than behind a flag.
  - **A host label can grant visibility.** With gate (b) an operator can only
    grant within what it already holds, which is precisely the Coven guarantee —
    but "who may attach labels" is now as load-bearing for traits as it has always
    been for covens.
  - **Prefer labelling the incarnation.** A label on the incarnation covers every
    host that joins it later, with no re-stamping; a host label covers exactly one
    VM. The recommended shape is to label the incarnation and bind hosts to it.
  - **Reads gain a correlated subquery.** Souls list/get resolve labels through
    `incarnation_membership`; the lookup rides the existing
    `incarnation_membership_sid_idx` and the GIN index on `incarnation.traits`.
  - **The M:N erasure bug disappears** with the projection that caused it: nothing
    writes a host's label column on behalf of an incarnation any more.

- **Rejected alternatives.**

  - **Keep the projection, switch REPLACE → merge.** Cheapest (no migration), and
    it does stop the immediate overwrite. Rejected: the projection cannot tell "the
    host owns this key" from "I wrote this key last time", so removing a key from
    `incarnation.traits` never removes it from the hosts. Stale org labels would
    linger forever inside an RBAC dimension — a permanent visibility leak — and the
    collision outcome would still be "whoever synced last".
  - **Two columns per host (host lane + projected lane) with a precedence rule.**
    Keeps day-2 propagation and makes overwriting structurally impossible, but it
    is still copying (now with bookkeeping), and it forces the precedence rule
    rejected in item 3.
  - **Per-host overrides declared on the incarnation (`spec.hosts[].traits`).**
    Preserves a single source of truth, but cannot label a host that belongs to no
    incarnation, has no answer for a host in two, and makes labelling one VM an
    edit of the incarnation.
  - **Fixing Trait only, leaving Coven as it is.** Rejected by the user: the two
    axes would carry different inheritance semantics, which is the confusion this
    ticket started from.

- **Amends.** [ADR-008](0008-coven-stable-tags.md) (the Coven axis gains
  inheritance by membership; NIM-124's split of membership from the coven column
  stands), [ADR-060](0060-traits.md) (the R1 materialized projection is
  superseded; items 1–4 and 6 of its read/target mechanics remain in force),
  [ADR-047](0047-purview.md) (the `coven` and `trait` dimensions of a souls-scope
  predicate resolve over inherited labels as well as own ones),
  [ADR-030](0030-vigil-oracle.md) (the `coven` half of a Vigil/Decree subject
  resolves over the union; the membership-check keeps reading the relation —
  NIM-224), [ADR-032](0032-push-orchestrator.md) (Level 2 of provider routing
  matches the union, with an own-before-inherited lookup order — NIM-251).
