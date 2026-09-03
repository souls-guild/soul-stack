package handlers

// THE INVARIANT, guarded where an incarnation row becomes an RBAC SCOPE
// ([ADR-0085], NIM-728): the `incarnation=` dimension is the identifier, and a
// caption reaches no dimension of the scope grammar.
//
// [IncarnationScopeSelector] is the one place the middleware turns a row into
// the context set every scoped permission is evaluated against, and it is the
// last point where a caption and an identifier are both in scope. A caption
// substituted here would be an authorization defect in both directions at once:
// a role scoped `incarnation=redis-billing` would stop matching the incarnation
// it was granted on, and would start matching whatever row an operator happened
// to caption `redis-billing` — so an operator with only the mutable field's
// permission could pull a foreign incarnation into their own scope.
//
// That is the sharpest reason the caption is mutable and the identifier is not.
//
// HOW TO BREAK IT ON PURPOSE (the mutation this file exists to catch):
// in IncarnationScopeSelector, pass `derefLabel(inc.Label)` instead of `inc.Name`
// to incarnationCovenContexts. Every test below goes red.
//
// The fixture caption is deliberately a VALID incarnation identifier — kebab, in
// range — so a naive substitution produces a well-formed scope value and no
// validation can be what fails. Only the assertions catch it.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// guardScopeID is the identifier: the `incarnation=` scope value.
	guardScopeID = "redis-billing"
	// guardScopeLabel is the caption. Different, and itself a valid identifier.
	guardScopeLabel = "redis-billing-prod"
	// guardScopeCoven is the one declared coven, so the test can tell a coven
	// dimension apart from the incarnation one.
	guardScopeCoven = "prod"
)

// guardLabelledIncRow — an incarnation row in scanIncarnation column order whose
// caption differs from its identifier.
func guardLabelledIncRow(name, label string) pgx.Row {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), "ready",
		[]byte(nil), any(nil),
		now, now, []string{guardScopeCoven},
		[]byte("{}"), // traits
		"create",     // created_scenario
		any(nil),     // applying_apply_id
		any(label),   // label (ADR-0085) — the subject of this guard
	}}
}

// TestIncarnationLabel_NotAnRBACScopeValue drives the real selector and pins that
// `incarnation=` is the identifier and the caption appears in no dimension.
func TestIncarnationLabel_NotAnRBACScopeValue(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row {
		return guardLabelledIncRow(name, guardScopeLabel)
	}}
	sel := IncarnationScopeSelector(db)
	req := newChiRequest(http.MethodPut, "/v1/incarnations/"+guardScopeID+"/label", nil, "name", guardScopeID)

	ctxs := sel(req)
	if len(ctxs) == 0 {
		t.Fatal("the selector produced no contexts — the guard would pass vacuously")
	}

	sawIdentifier := false
	for i, ctx := range ctxs {
		for dim, value := range ctx {
			if value == guardScopeLabel {
				t.Errorf("context %d: the CAPTION %q became the value of scope dimension %q.\n"+
					"ADR-0085: a caption participates in nothing derived, and an RBAC scope is one "+
					"of the four surfaces named. A caption here breaks authorization in BOTH "+
					"directions: a role scoped on the identifier stops matching, and an operator "+
					"who can edit the caption can pull a foreign incarnation into their own scope.",
					i, guardScopeLabel, dim)
			}
		}
		if ctx["incarnation"] == guardScopeID {
			sawIdentifier = true
		}
	}
	if !sawIdentifier {
		t.Errorf("no context carried `incarnation=%s` — the scope must be keyed on the IDENTIFIER; got %v",
			guardScopeID, ctxs)
	}
}

// TestIncarnationLabel_ScopeIgnoresCaptionChanges is the invariant as the
// operator experiences it: re-caption the incarnation, and every context the
// enforcer is handed is unchanged, so no grant starts or stops matching.
func TestIncarnationLabel_ScopeIgnoresCaptionChanges(t *testing.T) {
	contexts := func(t *testing.T, label string) []map[string]string {
		t.Helper()
		db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row {
			return guardLabelledIncRow(name, label)
		}}
		sel := IncarnationScopeSelector(db)
		req := newChiRequest(http.MethodPut, "/v1/incarnations/"+guardScopeID+"/label", nil, "name", guardScopeID)
		return sel(req)
	}

	before := contexts(t, guardScopeLabel)
	after := contexts(t, "Redis — Billing (production)")

	if len(before) != len(after) {
		t.Fatalf("the context set changed size when the caption changed: %d → %d", len(before), len(after))
	}
	for i := range before {
		if len(before[i]) != len(after[i]) {
			t.Fatalf("context %d changed shape when the caption changed: %v → %v", i, before[i], after[i])
		}
		for dim, want := range before[i] {
			if got := after[i][dim]; got != want {
				t.Errorf("context %d, dimension %q moved when the caption changed: %q → %q.\n"+
					"ADR-0085: \"I changed the label and nothing moved\" must be a guarantee, not a hope.",
					i, dim, want, got)
			}
		}
	}
}

// === Segment 3 of every derived state-secret path =========================
//
// THE INVARIANT at the derivation [ADR-0085] calls the sharpest of all:
// `<mount>/<service>/<incarnation>/<state-field>[/<key>]`
// ([ADR-0083] §1). `RevealSecretTyped` is the one place in the tree where that
// path is assembled from a LOADED registry row — so it is the one place where an
// incarnation's caption and its identifier are both in scope, and a one-token
// substitution (`inc.Name` → a deref'd `inc.Label` at
// incarnation_reveal_secrets.go) compiles and derives a perfectly well-formed
// DIFFERENT path.
//
// `SecretField.VaultPath` substitutes each segment verbatim and only checks it is
// segment-safe, so a kebab caption passes it without complaint.
//
// What DOES notice, on this path only, is the positive allowlist a few lines
// below the derivation: it is assembled independently from `inc.Name`, so a
// derivation that emitted something else fails the prefix check and the reveal
// answers 404 rather than reading a wrong location. That is a real second line
// of defence and this guard does not pretend otherwise — under the mutation
// above these tests go red at the CALL, not at the path assertion.
//
// The guard is still worth its place, for two reasons. It pins the derivation
// itself rather than the refusal, so a future change that relaxed or removed the
// allowlist would not quietly leave segment 3 unprotected; and the allowlist
// covers only the READ path. The WRITE path — `core.state.<verb>` minting and
// storing a secret at the same derived address — has no equivalent, and there a
// moved segment is exactly the silent orphan [ADR-0085] clause 1 describes. It
// is not substitutable in one token there only because no caption is in scope
// along it: the segments arrive as plain strings on the run context
// (`util.WithIncarnation`, from `scenario.RunSpec.IncarnationName`). That is a
// structural accident worth keeping, not a guarantee — which is why the
// narrow-struct assertions elsewhere in this change matter as much as the
// behavioural ones.
//
// The other consumer of this derivation, `core.state.<verb>`
// (keeper/internal/coremod/state), is reached only through strings placed on the
// run context (`util.WithIncarnation`, from `scenario.RunSpec.IncarnationName`)
// — no caption is in scope anywhere along it, which is why the guard lives here
// and not there.
//
// HOW TO BREAK IT ON PURPOSE: at incarnation_reveal_secrets.go, pass the
// incarnation's Label instead of `inc.Name` to `field.VaultPath`. Both tests
// below go red.

// guardRevealHandler is [revealHandler] with a caption on the loaded row.
func guardRevealHandler(t *testing.T, label string, vr VaultKVReader) *IncarnationHandler {
	t.Helper()
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row {
		return guardLabelledIncRowWithState(name, label, redisUsersState("alice"))
	}}
	h := NewIncarnationHandler(db, nil, nil, &fakeResolver{ok: true}, &fakeLoader{stateSchema: redisSecretSchema()},
		&fakeAuditWriter{}, fakeIncScoper{unrestricted: true}, nil)
	h.SetVaultReader(vr, "")
	return h
}

// guardLabelledIncRowWithState — an incarnation row carrying BOTH a state and a
// caption, in scanIncarnation column order.
func guardLabelledIncRowWithState(name, label string, state map[string]any) pgx.Row {
	stateBytes, _ := json.Marshal(state)
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		stateBytes, "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"), // traits
		"create",     // created_scenario
		any(nil),     // applying_apply_id
		any(label),   // label (ADR-0085) — the subject of this guard
	}}
}

// TestIncarnationLabel_NotTheStateSecretVaultSegment drives the real reveal path
// with a caption that differs from the identifier and pins that the address
// Vault is actually asked for carries the identifier.
func TestIncarnationLabel_NotTheStateSecretVaultSegment(t *testing.T) {
	// A caption that is itself a valid incarnation identifier AND a valid Vault
	// path segment: a substitution produces a well-formed path, so no format
	// check anywhere can be what fails.
	const caption = "redis-billing-prod"

	vr := &fakeVaultReader{data: map[string]any{"password": "s3cr3t-plaintext"}}
	h := guardRevealHandler(t, caption, vr)

	if _, err := h.RevealSecretTyped(context.Background(), revealClaims(), guardScopeID, userPasswordID, "alice"); err != nil {
		t.Fatalf("RevealSecretTyped: %v\n"+
			"A refusal here most likely means the DERIVATION moved off the identifier and the "+
			"independent allowlist below it caught the mismatch — which is the invariant this "+
			"file exists for, arriving one step earlier than the path assertion below.", err)
	}
	if len(vr.calledWith) != 1 {
		t.Fatalf("ReadKV called %d times, want 1 — the guard would pass vacuously", len(vr.calledWith))
	}
	got := vr.calledWith[0]
	want := "secret/redis/" + guardScopeID + "/redis_users/alice"
	if got != want {
		t.Errorf("the derived Vault path is %q, want %q\n"+
			"ADR-0085: an incarnation's label participates in nothing derived, and segment 3 of "+
			"this path is the identifier. `SecretField.VaultPath` substitutes verbatim and folds "+
			"no case, and secretwrite REPLACES rather than merges — a caption here relocates "+
			"every secret of the incarnation on every label edit, and the new path is perfectly "+
			"well-formed and simply empty. No error is raised anywhere.", got, want)
	}
	if strings.Contains(got, caption) {
		t.Errorf("the CAPTION %q reached the derived Vault path %q", caption, got)
	}
}

// TestIncarnationLabel_StateSecretPathIgnoresCaptionChanges is the invariant as
// the operator experiences it: re-caption the incarnation, and the address Vault
// is asked for is byte-identical, so every password already issued stays
// reachable.
func TestIncarnationLabel_StateSecretPathIgnoresCaptionChanges(t *testing.T) {
	derive := func(t *testing.T, label string) string {
		t.Helper()
		vr := &fakeVaultReader{data: map[string]any{"password": "s3cr3t-plaintext"}}
		h := guardRevealHandler(t, label, vr)
		if _, err := h.RevealSecretTyped(context.Background(), revealClaims(), guardScopeID, userPasswordID, "alice"); err != nil {
			t.Fatalf("RevealSecretTyped: %v", err)
		}
		if len(vr.calledWith) != 1 {
			t.Fatalf("ReadKV called %d times, want 1", len(vr.calledWith))
		}
		return vr.calledWith[0]
	}

	before := derive(t, "redis-billing-prod")
	after := derive(t, "Redis — Billing (production)")
	if before != after {
		t.Errorf("the derived secret path moved when the caption changed: %q → %q.\n"+
			"ADR-0085: \"I changed the label and nothing moved\" must be a guarantee, not a hope — "+
			"and here the cost of it not being one is an orphaned password.", before, after)
	}
}

// TestLabelWriteReply_AuditPayloadRecordsBothSides pins the shared audit payload
// of all ten label routes: the identifier that was ADDRESSED, plus the caption on
// BOTH sides of the change, each explicitly null where it was absent.
//
// Both sides, not just the new one, because the event is named after
// `incarnation.traits_changed` — which records `{name, old_keys, new_keys}` — and
// an audit line that cannot say what a value used to be cannot answer the
// question an investigation actually asks.
func TestLabelWriteReply_AuditPayloadRecordsBothSides(t *testing.T) {
	before, after := "redis-billing", guardScopeLabel
	p := LabelWriteReply[ProviderView]{ID: guardScopeID, Previous: &before, Label: &after}.AuditPayload()

	if p["id"] != guardScopeID {
		t.Errorf("audit `id` = %v, want the identifier %q", p["id"], guardScopeID)
	}
	if got, _ := p["old_label"].(*string); got == nil || *got != before {
		t.Errorf("audit `old_label` = %v, want %q", p["old_label"], before)
	}
	if got, _ := p["new_label"].(*string); got == nil || *got != after {
		t.Errorf("audit `new_label` = %v, want %q", p["new_label"], after)
	}
	// The identifier is what was addressed, never what changed — there is no
	// rename operation anywhere.
	if p["id"] == p["new_label"] {
		t.Error("the audit payload conflates the identifier with the caption")
	}

	// Both keys present and explicitly nil at the ends of the range: setting a
	// caption on a row that had none, and clearing one that did. An omitted key
	// would make either indistinguishable from "this event predates the field".
	firstEver := LabelWriteReply[ProviderView]{ID: guardScopeID, Label: &after}.AuditPayload()
	if _, present := firstEver["old_label"]; !present {
		t.Error("a first-ever caption omitted `old_label` entirely")
	}
	if v, _ := firstEver["old_label"].(*string); v != nil {
		t.Errorf("a first-ever caption recorded old_label=%v, want an explicit nil", v)
	}

	cleared := LabelWriteReply[ProviderView]{ID: guardScopeID, Previous: &before}.AuditPayload()
	if _, present := cleared["new_label"]; !present {
		t.Error("a cleared caption omitted `new_label` entirely")
	}
	if v, _ := cleared["new_label"].(*string); v != nil {
		t.Errorf("a cleared caption recorded new_label=%v, want an explicit nil", v)
	}
	if got, _ := cleared["old_label"].(*string); got == nil || *got != before {
		t.Errorf("a cleared caption lost what it was cleared FROM: old_label=%v, want %q",
			cleared["old_label"], before)
	}
}
