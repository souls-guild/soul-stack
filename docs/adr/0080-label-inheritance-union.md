# ADR-080. Coven and Trait — one label world: inheritance by membership, union at read

> ## ⛔ REVERTED (2026-08-05, NIM-281) — inheritance does not exist
>
> **A host's labels are exactly the ones an operator attached to that host** —
> `souls.coven[]` and `souls.traits`. Belonging to an incarnation attaches
> nothing: no copy, no read-time union, no projection, and never the
> incarnation's name. The rule this ADR decided was built (NIM-121) and is now
> removed in full, on both axes and in every reader.
>
> The replacement is the
> [NIM-281 amendment of ADR-008](0008-coven-stable-tags.md#amendment-2026-08-05-nim-281-a-label-is-never-inherited),
> which is the live text. In short: `coven=` is a label question answered from
> `souls.coven` alone; reaching an incarnation's hosts is a MEMBERSHIP question,
> spelled `incarnation=<name>` and answered from `incarnation_membership`. The
> incarnation-side predicate lost its `name = ANY($x)` arm for the same reason —
> a name is an identity, not a label.
>
> The one thing that survives is **not** inheritance and never was: an
> incarnation's own covens select overlays of the incarnation's OWN config, via a
> `foreach:` over `incarnation.covens` in `vars/_stack.yaml`
> ([ADR-0082](0082-service-vars.md)). Membership decides which incarnation's
> config a host is owed; the incarnation's labels decide which overlays of that
> config apply. No host label is involved on either step.
>
> Known narrowing left open: a Vigil/Decree or Augur Rite subject is `sid` XOR
> `coven`, so nothing can now bind one to "every member of incarnation X" without
> tagging those hosts by hand — **NIM-280**.
>
> **Everything below is kept as the record of a decision that was made, built and
> withdrawn. Do not implement from it.**

- **Status.** Superseded (2026-08-05, NIM-281 — reverted in full). Superseded the
  R1 relocation amendment of [ADR-060](0060-traits.md) (Trait per-soul →
  per-incarnation with a materialized projection) and extended
  [ADR-008](0008-coven-stable-tags.md) with the same rule for the Coven axis.
  Landed by NIM-121. Note that the ADR-060 R1 projection is **not** restored by
  the revert: `SyncTraitsToHosts` stays deleted and migration
  [106](../../keeper/migrations/106_prune_projected_soul_traits.up.sql) stays
  applied — copying a label onto a host is exactly what NIM-281 forbids.

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
     - **Augur subject binding** ([ADR-025](0025-augur.md), added by NIM-249) —
       the `coven` half of a Rite's subject, on the same grounds: ADR-025 writes a
       Rite's subject the way ADR-030 writes a Decree's. This one failed loudly
       (a subject matching no Rite is default-denied, so hosts were refused
       mid-apply) where the reactor failed silently.
     - **Telemetry essence overlays** ([ADR-072](0072-host-utilization.md), added
       by NIM-248) — the coven layers of the effective telemetry config resolve
       over the union, so a tag put on the incarnation reaches its members'
       essence. Which incarnation's config a host is owed in the first place is a
       membership question and is answered from the relation (see below).
     - **The operator-facing coven filter and the bulk path** (added by NIM-250)
       — `GET /v1/souls?coven=`, the `selector.coven` of `soul.coven-assign` /
       `soul.traits-assign`, and **scope gate (a)** of those bulk calls ("target
       hosts ⊆ the operator's coven-scope") all render the SAME predicate the
       RBAC pushdown does, from one implementation (`rbac.CovenScopeSQL`). These
       are not a fourth reader with its own opinion: the set an operator can
       find, the set a bulk call selects, and the set authorizing that call are
       answers to one question. Matching the raw column in them while the scope
       resolved the union meant an operator could be granted a host, see it in
       the list, and then neither find it by the label that made it visible nor
       change it — `matched` came back short with nothing to explain it.

       Gate (a) widening is a real change to what a deployed role may WRITE, and
       it is deliberate: the read boundary and the write boundary of the same
       scope must be the same set. **Gate (b) is untouched** — the label being
       attached must still lie inside the operator's own coven-scope, so nobody
       gains the ability to hand a host to a foreign role.

       This is also where `coven=` and `incarnation=` are held apart. `coven=` is
       a **label** question and matches the union, so a host carrying a tag
       spelled like an incarnation matches it — it genuinely carries that label.
       `incarnation=` is a **membership** question and keeps reading
       `incarnation_membership` (see the boundary below); the two selectors are
       not two spellings of one thing, and neither subsumes the other.
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
     with no error anywhere. It has now happened five times over the same axis
     (NIM-224, NIM-248, NIM-249, NIM-250, NIM-251), which is why the union is no
     longer open-coded per consumer. It has **two entry points, one per layer**,
     and a consumer that reaches past both is a bug by construction:

     - **`soul.EffectiveCovens(ctx, db, sid)`** — asks "which covens does THIS
       host carry", in Go, for one SID. Reading `SelectBySID(...).Coven` in order
       to match it against something an operator wrote is the defect above.
     - **`rbac.CovenScopeSQL(covenCol, membershipSID, $vals)`** — asks "which
       ROWS carry any of these covens", as a SQL predicate (NIM-250). Set-based
       readers — the RBAC pushdown, the souls list filter, the bulk selector and
       bulk scope gate (a) — need the answer inside the query: they page with
       exact offset/total and iterate by keyset, so resolving per host in Go
       would be an N+1 and would break both. Writing `$1 = ANY(coven)` by hand is
       the same defect wearing SQL.

     These are one resolution expressed at two layers, not two policies, and they
     are pinned to each other by tests rather than by intent: the `soul` package's
     filter/selector/gate assertions compare against `rbac.CovenScopeSQL` output
     verbatim, so the two cannot drift into disagreeing about a host.

     Routing is the one deliberate exception to BOTH and must stay one: it needs
     the two halves kept apart to order its lookup, so it takes them from the same
     `LoadInheritedLabels` without collapsing them. Collapsing it "for
     consistency" would destroy the own-before-inherited tiebreak.

     **Membership is not a label question, and must never be answered from the
     union.** `incName ∈ effectiveCovens` holds both for a member and for a host
     merely carrying a host-attached tag spelled like the incarnation's name, so
     any gate deciding *belonging* — the Oracle's cross-incarnation guard, the
     Choir voice invariant, the roster, telemetry delivery — reads
     `incarnation_membership` directly. The union widens what a rule may **see**;
     only the relation says where a host **belongs**. Conflating them turns this
     ADR's widening into an escalation path.

     The two questions can meet inside one resolve without contradiction, and
     telemetry is the worked example: membership picks the incarnation whose
     service config the host is owed, then the union decides which coven overlays
     of that config apply to it. Read the relation for *which*, the union for
     *how much*.

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
  - **The bulk write boundary widens with it** (NIM-250). Scope gate (a) of
    `soul.coven-assign` / `soul.traits-assign` admits the same hosts the read
    scope admits, so a role scoped `coven=<X>` can now label the hosts of a
    matching incarnation. This follows from the item above rather than extending
    it: a boundary that authorizes reads over the union and writes over the raw
    column is two different scopes wearing one name. Gate (b) still bounds WHICH
    label may be attached, so the widening cannot be used to reach outside the
    operator's own scope.
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
    edit of the incarnation. (Moot since 2026-07-30: `spec.hosts[]` itself is
    removed, [ADR-044 amendment](0044-choir.md#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role).)
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
  NIM-224), [ADR-025](0025-augur.md) (the `coven` half of a Rite's subject
  resolves over the union — NIM-249), [ADR-072](0072-host-utilization.md)
  (telemetry delivery resolves the host's incarnation through
  `incarnation_membership` and its essence coven overlays through the union —
  NIM-248), [ADR-032](0032-push-orchestrator.md) (Level 2 of provider routing
  matches the union, with an own-before-inherited lookup order — NIM-251).
