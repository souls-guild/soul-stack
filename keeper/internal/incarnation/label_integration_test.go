//go:build integration

// The display caption against a real Postgres, on the entity whose column list
// is the longest and whose caption is the most consequential ([ADR-0085],
// NIM-728, migration `117`).
//
// Alignment of the INSERT / SELECT / scan lists for all ten tables is already
// covered by the pre-existing L1 suites — they insert and read real rows, so a
// misordered column fails them. What is NOT covered by those is the SEMANTICS
// of the new field, which is what this file and its provider twin add:
// round-trip, clear-to-NULL through both spellings, and the invariant stated as
// the operator experiences it.
//
// Incarnation is worth its own copy rather than trusting the provider one for
// two reasons: it carries fifteen scanned columns to provider's eight (so
// "the caption reads back" is a stronger statement here), and its identifier is
// segment 3 of every derived secret path, the RBAC `incarnation=` scope value
// and the CEL root at once — so "the caption moved and the identifier did not"
// is worth asserting on a live row and not only over fakes.

package incarnation

import (
	"context"
	"strings"
	"testing"
)

func labelledIncarnation(name, aid, label string) *Incarnation {
	return &Incarnation{
		Name:               name,
		Label:              &label,
		Service:            "redis",
		ServiceVersion:     "v1.0.0",
		StateSchemaVersion: 1,
		State:              map[string]any{"primary": name + "-01"},
		Status:             StatusReady,
		CreatedByAID:       &aid,
	}
}

// TestIntegration_Label_RoundTrip pins the column end to end through fifteen
// scanned columns: written on create, read back by name and by the list path.
func TestIntegration_Label_RoundTrip(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	// Capitals, spaces and an em-dash — free text is the whole point, and a CHECK
	// on the column's form would fail this insert and nothing else.
	const caption = "Redis — Billing (production)"
	inc := labelledIncarnation("redis-billing", "archon-alice", caption)
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create with a caption: %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, "redis-billing")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Label == nil || *got.Label != caption {
		t.Fatalf("Label after round-trip = %v, want %q — the INSERT list, the SELECT list and "+
			"the scan targets must agree, which only a real query can show", got.Label, caption)
	}
	if got.Name != "redis-billing" || got.Service != "redis" {
		t.Errorf("the row moved while carrying a caption: name=%q service=%q", got.Name, got.Service)
	}

	// The list path has its own SQL and its own scan; a mismatch there is a
	// separate bug from the by-name path.
	items, total, err := SelectAll(ctx, integrationPool, ListFilter{}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("SelectAll returned %d/%d, want 1/1", len(items), total)
	}
	if items[0].Label == nil || *items[0].Label != caption {
		t.Errorf("Label from the list path = %v, want %q", items[0].Label, caption)
	}
}

// TestIntegration_Label_AbsentIsNull — a row created without a caption reads back
// nil, so a consumer has one condition to test before falling back.
func TestIntegration_Label_AbsentIsNull(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-billing", Service: "redis", ServiceVersion: "v1.0.0",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create without a caption: %v", err)
	}
	got, err := SelectByName(ctx, integrationPool, "redis-billing")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Label != nil {
		t.Errorf("Label = %q for a row created without one, want nil (SQL NULL)", *got.Label)
	}
}

// TestIntegration_UpdateLabel_ChangesOnlyTheCaption is the invariant on a live
// row: after the write the caption is new and everything the platform derives
// from is byte-identical — including `name`, which is simultaneously segment 3
// of every derived secret path, the RBAC scope value and the CEL root.
func TestIntegration_UpdateLabel_ChangesOnlyTheCaption(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := Create(ctx, integrationPool, labelledIncarnation("redis-billing", "archon-alice", "Redis (old)")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, err := SelectByName(ctx, integrationPool, "redis-billing")
	if err != nil {
		t.Fatalf("SelectByName(before): %v", err)
	}

	const renamed = "Redis — Billing (production)"
	label := renamed
	if _, err := UpdateLabel(ctx, integrationPool, "redis-billing", &label); err != nil {
		t.Fatalf("UpdateLabel: %v", err)
	}
	after, err := SelectByName(ctx, integrationPool, "redis-billing")
	if err != nil {
		t.Fatalf("SelectByName(after): %v", err)
	}

	if after.Label == nil || *after.Label != renamed {
		t.Errorf("Label = %v, want %q", after.Label, renamed)
	}
	switch {
	case after.Name != before.Name:
		t.Errorf("name moved: %q → %q — it is the Vault path segment, the RBAC scope value AND "+
			"the CEL root, and there is no rename operation anywhere", before.Name, after.Name)
	case after.Service != before.Service || after.ServiceVersion != before.ServiceVersion:
		t.Errorf("service coordinates moved: %q@%q → %q@%q",
			before.Service, before.ServiceVersion, after.Service, after.ServiceVersion)
	case after.Status != before.Status:
		t.Errorf("status moved: %q → %q — a caption edit is not a lifecycle event", before.Status, after.Status)
	case !after.UpdatedAt.Equal(before.UpdatedAt):
		t.Errorf("updated_at moved: %v → %v — a caption edit must not read as a change to the "+
			"incarnation's substance in a list an operator triages by", before.UpdatedAt, after.UpdatedAt)
	}
	if len(after.State) != len(before.State) {
		t.Errorf("state changed shape: %v → %v", before.State, after.State)
	}
}

// TestIntegration_UpdateLabel_AllowedWhileApplying pins the deliberate absence of
// a status gate: unlike every other write on this table, a caption edit touches a
// column no run reads, so an operator fixing a typo is not blocked by a stuck run
// and does not disturb a live one.
func TestIntegration_UpdateLabel_AllowedWhileApplying(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	inc := labelledIncarnation("redis-billing", "archon-alice", "Redis")
	inc.Status = StatusApplying
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create(applying): %v", err)
	}

	label := "Redis — Billing"
	if _, err := UpdateLabel(ctx, integrationPool, "redis-billing", &label); err != nil {
		t.Fatalf("UpdateLabel while applying: %v\n"+
			"ADR-0085: there is deliberately no status gate — no run reads the caption.", err)
	}
	got, err := SelectByName(ctx, integrationPool, "redis-billing")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusApplying {
		t.Errorf("status = %q after a caption edit, want it untouched at %q", got.Status, StatusApplying)
	}
}

// TestIntegration_UpdateLabel_ClearsToNull — both spellings of "no caption" reach
// the same NULL.
func TestIntegration_UpdateLabel_ClearsToNull(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := Create(ctx, integrationPool, labelledIncarnation("redis-billing", "archon-alice", "Redis")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	blank := "   "
	for _, tc := range []struct {
		name  string
		label *string
	}{
		{"nil clears", nil},
		{"blank clears", &blank},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := "Redis"
			if _, err := UpdateLabel(ctx, integrationPool, "redis-billing", &restore); err != nil {
				t.Fatalf("UpdateLabel(restore): %v", err)
			}
			if _, err := UpdateLabel(ctx, integrationPool, "redis-billing", tc.label); err != nil {
				t.Fatalf("UpdateLabel(clear): %v", err)
			}
			got, err := SelectByName(ctx, integrationPool, "redis-billing")
			if err != nil {
				t.Fatalf("SelectByName: %v", err)
			}
			if got.Label != nil {
				t.Errorf("Label = %q after clearing, want nil", *got.Label)
			}
		})
	}
}

// TestIntegration_UpdateLabel_NotFound — the sentinel a handler maps to 404.
func TestIntegration_UpdateLabel_NotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	label := "whatever"
	_, err := UpdateLabel(ctx, integrationPool, "no-such-incarnation", &label)
	if err == nil {
		t.Fatal("UpdateLabel on a missing row returned nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("UpdateLabel error = %v, want the not-found sentinel", err)
	}
}

// TestIntegration_Label_IsNotUnique — two incarnations may carry the same
// caption. A caption is not an address.
func TestIntegration_Label_IsNotUnique(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	const shared = "Redis — Production"
	if err := Create(ctx, integrationPool, labelledIncarnation("redis-billing", "archon-alice", shared)); err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	if err := Create(ctx, integrationPool, labelledIncarnation("redis-orders", "archon-alice", shared)); err != nil {
		t.Fatalf("Create(second) with a duplicate caption: %v\n"+
			"ADR-0085: label is deliberately NOT unique.", err)
	}
}

// TestIntegration_UpdateLabel_ReturnsThePreviousCaption is the reason the write
// is an `UPDATE … RETURNING old.label` rather than a read followed by a write:
// the audit event records `{name, old_label, new_label}`, and both halves have
// to describe ONE transition.
//
// A `FROM <table> AS old` self-join is not an obvious construction, and its whole
// point is invisible over a fake — a fake returns whatever it was told to. Only a
// real Postgres can show that the alias reads the PRE-update snapshot rather than
// the row the same statement just wrote.
func TestIntegration_UpdateLabel_ReturnsThePreviousCaption(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	const first = "Redis (staging)"
	if err := Create(ctx, integrationPool, labelledIncarnation("redis-billing", "archon-alice", first)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// (a) caption → caption: the old value is what the row held, NOT the new one.
	const second = "Redis — Billing (production)"
	next := second
	previous, err := UpdateLabel(ctx, integrationPool, "redis-billing", &next)
	if err != nil {
		t.Fatalf("UpdateLabel: %v", err)
	}
	if previous == nil || *previous != first {
		t.Fatalf("previous caption = %v, want %q.\n"+
			"If this equals the NEW caption, the RETURNING alias is reading the row the same "+
			"statement wrote instead of the pre-update snapshot, and every audit event in the "+
			"family records a transition from a value to itself.", previous, first)
	}
	if *previous == second {
		t.Error("the previous caption equals the new one — the self-join is not reading the snapshot")
	}

	// (b) caption → cleared: the old value survives the clear.
	cleared, err := UpdateLabel(ctx, integrationPool, "redis-billing", nil)
	if err != nil {
		t.Fatalf("UpdateLabel(clear): %v", err)
	}
	if cleared == nil || *cleared != second {
		t.Errorf("clearing reported previous=%v, want %q — an audit line that cannot say what a "+
			"caption was cleared FROM answers no question worth asking", cleared, second)
	}

	// (c) absent → caption: nil old value, which the audit records as an explicit
	// null rather than an omitted key.
	back := first
	fromNothing, err := UpdateLabel(ctx, integrationPool, "redis-billing", &back)
	if err != nil {
		t.Fatalf("UpdateLabel(set again): %v", err)
	}
	if fromNothing != nil {
		t.Errorf("setting a caption on a row that had none reported previous=%q, want nil", *fromNothing)
	}

	// (d) the row really ends where the last call said it did.
	got, err := SelectByName(ctx, integrationPool, "redis-billing")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Label == nil || *got.Label != first {
		t.Errorf("final caption = %v, want %q", got.Label, first)
	}
}
