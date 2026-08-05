## ADR-0081. Roster at create — a scenario declares which input carries its hosts

**Status:** active
**Amends:** [ADR-008](0008-coven-stable-tags.md) (amendment 2026-07-28 / NIM-209, operator membership), [ADR-044](0044-choir.md) S-T1 (the `source:` catalog), [ADR-045](0045-param-dsl.md) S4 (the SID picker)
**Implemented by:** NIM-371

### Problem

A create scenario that deploys onto hosts somebody already onboarded — `create_from_souls`
in the shipped redis example — had no way to be given those hosts.

- `POST /v1/incarnations` accepted `name` / `service` / `covens` / `input` / `traits` /
  `create_scenario`. No hosts.
- Hosts were bound AFTER the fact, through `POST /v1/incarnations/{name}/members`
  (ADR-008 amendment / NIM-209).
- But the bootstrap run starts DURING the create (`lifecycle.auto_create`), and a run
  resolves its roster from `incarnation_membership` at start. An empty relation aborts it
  with `no_hosts` (`scenario/run.go`).

So the only reachable sequence was: create (the run fails), bind the roster, run the
scenario by hand. The web create form said as much, in a hint advising the operator to
"add them via the Hosts tab before running" — that is, offering to create an incarnation
whose run had already gone nowhere.

A scenario cannot fix this from the inside. Binding is an act, and the run aborts before
its first task; that is precisely what NIM-209 was opened on.

### Decision

**A create scenario DECLARES which of its `input:` fields carries its roster**, with a
third variant of the ADR-044 S-T1 source discriminator:

```yaml
input:
  hosts:
    type: array
    required: true
    min_items: 1
    items:
      type: string
      format: sid
      source: { roster: true }
```

The key does two jobs, deliberately:

- **For the UI** it is where the SID catalog comes from. Unlike `incarnation_hosts` and
  `choir`, which resolve against an incarnation that already exists, this one resolves
  the onboarded, online souls the caller may see — a create form has no incarnation to
  ask about.
- **For Keeper** it is the statement that this input IS the composition of the
  incarnation. `POST /v1/incarnations` binds the field's value into
  `incarnation_membership` after inserting the row and **before** starting the bootstrap
  run.

WHICH field it is on is the scenario author's choice; Keeper finds it by the key, never by
a blessed field name (`config.RosterInputField`). At most one field per `input:` block may
carry it — a second has no defined meaning, and the config validator rejects it
(`input_roster_source_duplicate`).

**The contract of `POST /v1/incarnations` does not change.** The roster travels inside
`input`, validated by the ordinary input gate: `required` makes it mandatory, `min_items`
/ `format: sid` check its shape, and a `validate:` rule can check its SIZE against the
declared topology on the request path — a 422 instead of an `error_locked` from the
render-time assert.

#### Order, and why it is the whole decision

```
resolve plan (input gate; roster extracted from the merged input)
screen roster            ← every refusal happens here, BEFORE anything exists
incarnation.Create       ← the FK the membership rows need
bind roster              ← incarnation_membership
runner.Start             ← the run now resolves a roster that is already there
```

Screening before the insert is what keeps a refusal from leaving a half-made incarnation
behind. Binding after it is forced by the FK. Binding before the run is the point of the
feature.

If the bind fails for an infrastructural reason, the create answers 500 and **no run
starts**: the incarnation stays behind, empty and `ready`, which the operator repairs with
`POST .../members` + run. Starting a run onto a roster we know is incomplete would hand
them an `error_locked` to unlock first.

#### Authorization — the same two gates as the bind route

1. **`incarnation.bind-member`** over the incarnation being created, ANDed over every
   declared coven (`ScreenIncarnationCreateRosterScope`). Asked only when the request
   carries a roster. Without it, create would be the way around NIM-209's gate (a): a
   caller holding only `incarnation.create` could compose at birth a roster they would be
   refused a minute later.
2. **Each SID inside the caller's `soul.list` purview**, all-or-nothing
   (`incarnation.ScreenBindCandidates`, shared verbatim with the bind route). Bucket order
   is a security property, not cosmetics: unknown → 422, out-of-scope → **403**,
   not-connected → 422. An operator who may not see a host is told "forbidden", never that
   it is disconnected.

Both REST and MCP run the same screening function, for the same reason gate (b) of create
was extracted in NIM-333: two implementations of one boundary drift.

#### `input.hosts` is a journal; membership is the truth

The input value records what the incarnation was created ON — write-once, like every other
input. The live composition is `incarnation_membership`, edited afterwards through the
Hosts tab. The two legitimately diverge the moment a host is added or removed, and every
LATER run resolves the relation, never the field. There is no reconciliation between them
and no arbiter needed: they answer different questions.

This is also why the roster is NOT written into `spec.hosts`. That field declares host
ROLES for the topology resolver (ADR-008); it does not make a host a member, so writing
the roster there would read plausible and bind nothing.

#### The source catalog

`GET /v1/souls` answers it, with two additions rather than a new endpoint:

- `sid_prefix` — autocomplete, matched literally (LIKE metacharacters escaped);
- `coven` becomes repeatable and matches ANY of the labels given — which its own
  documentation already promised while the implementation took one value.

**The picker narrows by ONE thing: `status=connected`.** That is an invariant — the
keeper refuses to bind anything else, so offering it would be offering a 422. Two
tighter filters were built first and are both wrong:

- **by the incarnation's declared covens.** A host never gets those labels at all — binding
  it attaches nothing ([NIM-281](0008-coven-stable-tags.md#amendment-2026-08-05-nim-281-a-label-is-never-inherited)),
  and only an operator tagging it by hand ever will. Filtering by them would hide every
  candidate nobody had hand-tagged, which on a fresh fleet is all of them. The covens an
  operator declares describe the incarnation, not a requirement on its future hosts.
- **by "unassigned".** Membership is M:N (NIM-124): a host legitimately serves several
  incarnations, so serving one is no reason to hide it from another. On a six-host fleet
  with three already in use the picker offered three, with nothing on screen to say where
  the others went.

Both were removed after the first live review. What keeps other people's hosts out of the
list is the RBAC scope, not a heuristic about occupancy.

`unassigned` stays on the endpoint — "which hosts are in no incarnation at all" is a fair
question to ask the registry — but nothing in the create form asks it.

Reusing the souls list rather than extending `POST /v1/modules/{name}/form-prep` is the
load-bearing choice here. form-prep is addressed per module (`/v1/modules/{name}/…`) and a
create form has no module. More importantly, the souls list is ALREADY scoped by
`soul.list` and fail-closed, so "the picker cannot show a SID the caller could not
otherwise see" holds **by construction** — the same code, not a second copy of the rule.
An enumerator of the whole park behind an autocomplete is the failure class of NIM-148
(existence oracle) and NIM-202/203 ("visible ⟺ grantable"); the cheapest way not to
reintroduce it is to have no second implementation to keep in step.

### Rejected

- **A `hosts[]` field in the `POST /v1/incarnations` body.** Works, and was the first
  proposal, but it puts the roster in the core contract for every service while only a
  `*_from_souls` scenario needs it — and the scenario is what knows whether it is given
  hosts or produces them. The declaration belongs where that knowledge is.
- **Writing the roster to `spec.hosts`.** Binds nothing (see above).
- **Letting the scenario bind its own roster** from `input`. Impossible: the run aborts
  `no_hosts` before its first task.
- **A separate `requires_roster` scenario flag** (the shape of `composes_name`, ADR-0079).
  Redundant — the presence of a `source: { roster: true }` field IS the flag, and one
  source of truth cannot disagree with itself. It also let the web form stop guessing from
  the scenario NAME (`name.includes('from_souls')`).
- **Narrowing the catalog by occupancy or by the declared covens.** See above: the first
  contradicts M:N membership, the second asks a candidate to carry a label it can only
  get by being chosen.
- **Operator-assigned master/replica roles in the create form.** The topology already
  covers it: `cluster_topology` lets an operator declare the shard layout explicitly, and
  without it the plugin lays roles out by sorted SID. Roles stay editable afterwards via
  `PATCH .../hosts`.
