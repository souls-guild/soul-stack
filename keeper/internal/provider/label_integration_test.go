//go:build integration

// The display caption against a real Postgres ([ADR-0085], NIM-728, migration
// `117`). Everything else about `label` is checked over fakes, which can only
// prove that the Go code is self-consistent: a fake row scanner replays whatever
// column order the test author wrote, so a mismatch between the INSERT list, the
// SELECT list and the scan targets survives every unit test in the tree and
// fails on the first real query.
//
// This is the tier that can tell, and it is deliberately about the COLUMN and
// its semantics rather than the endpoint:
//
//   - the column exists, is nullable, and takes free text;
//   - it round-trips through Insert → SelectByID → SelectAll;
//   - UpdateLabel changes it and nothing else on the row;
//   - blank collapses to SQL NULL, so "absent" has one representation;
//   - nothing about it is unique — two rows may carry the same caption.
//
// Provider is the registry under test because it is the smallest one that has a
// caption AND a derived Vault path, so "the caption moved, the derivation did
// not" is observable in one table.

package provider

import (
	"context"
	"strings"
	"testing"
)

// labelledProvider is [newProvider] with a caption attached.
func labelledProvider(name, aid, label string) *Provider {
	p := newProvider(name, aid)
	p.Label = &label
	return p
}

// TestIntegration_Label_RoundTrip pins the column end to end: written on insert,
// read back by both select paths, and unchanged by anything else.
func TestIntegration_Label_RoundTrip(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	// A caption with capitals, spaces and punctuation — the whole reason the
	// field exists beside the kebab identifier. If migration 117 had carried a
	// CHECK on its form, this insert would fail here and nowhere else.
	const caption = "AWS — Production (Frankfurt)"
	p := labelledProvider("aws-eu", "archon-alice", caption)
	if err := Insert(ctx, integrationPool, p); err != nil {
		t.Fatalf("Insert with a caption: %v", err)
	}

	got, err := SelectByID(ctx, integrationPool, "aws-eu")
	if err != nil {
		t.Fatalf("SelectByID: %v", err)
	}
	if got.Label == nil || *got.Label != caption {
		t.Fatalf("Label after round-trip = %v, want %q — the INSERT column list, the SELECT "+
			"column list and the scan targets must agree, which only a real query can show",
			got.Label, caption)
	}
	// The identifier and the derivation ingredients are untouched by carrying one.
	if got.ID != "aws-eu" || got.CredentialsRef != "vault:secret/cloud/aws-eu" {
		t.Errorf("the row moved while carrying a caption: %+v", got)
	}

	// The list path scans through the same column list; a mismatch there is a
	// separate bug from the by-name path and has its own SQL.
	all, total, err := SelectAll(ctx, integrationPool, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 1 || len(all) != 1 {
		t.Fatalf("SelectAll returned %d/%d, want 1/1", len(all), total)
	}
	if all[0].Label == nil || *all[0].Label != caption {
		t.Errorf("Label from the list path = %v, want %q", all[0].Label, caption)
	}
}

// TestIntegration_Label_AbsentIsNull pins the other half of the round-trip: a row
// written without a caption reads back nil rather than "", so a consumer has one
// condition to test before falling back to the identifier.
func TestIntegration_Label_AbsentIsNull(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newProvider("aws-eu", "archon-alice")); err != nil {
		t.Fatalf("Insert without a caption: %v", err)
	}
	got, err := SelectByID(ctx, integrationPool, "aws-eu")
	if err != nil {
		t.Fatalf("SelectByID: %v", err)
	}
	if got.Label != nil {
		t.Errorf("Label = %q for a row written without one, want nil (SQL NULL)", *got.Label)
	}
}

// TestIntegration_UpdateLabel_ChangesOnlyTheCaption is the mutation as an
// operator performs it, and the assertion the whole field exists for: after the
// write the caption is new and every other column — including the identifier the
// Vault path is derived from — is byte-identical.
func TestIntegration_UpdateLabel_ChangesOnlyTheCaption(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	p := labelledProvider("aws-eu", "archon-alice", "AWS (old)")
	if err := Insert(ctx, integrationPool, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	before, err := SelectByID(ctx, integrationPool, "aws-eu")
	if err != nil {
		t.Fatalf("SelectByID(before): %v", err)
	}

	const renamed = "AWS — Production (Frankfurt)"
	label := renamed
	if _, err := UpdateLabel(ctx, integrationPool, "aws-eu", &label); err != nil {
		t.Fatalf("UpdateLabel: %v", err)
	}

	after, err := SelectByID(ctx, integrationPool, "aws-eu")
	if err != nil {
		t.Fatalf("SelectByID(after): %v", err)
	}
	if after.Label == nil || *after.Label != renamed {
		t.Errorf("Label = %v, want %q", after.Label, renamed)
	}
	// ADR-0085: "I changed the label and nothing moved." Everything the platform
	// derives from lives in these columns.
	switch {
	case after.ID != before.ID:
		t.Errorf("name moved: %q → %q — it is the derived Vault path segment and has no rename operation",
			before.ID, after.ID)
	case after.Type != before.Type || after.Region != before.Region:
		t.Errorf("type/region moved: %q/%q → %q/%q", before.Type, before.Region, after.Type, after.Region)
	case after.CredentialsRef != before.CredentialsRef:
		t.Errorf("credentials_ref moved: %q → %q — the caption edit relocated a secret",
			before.CredentialsRef, after.CredentialsRef)
	case !after.CreatedAt.Equal(before.CreatedAt):
		t.Errorf("created_at moved: %v → %v", before.CreatedAt, after.CreatedAt)
	}
}

// TestIntegration_UpdateLabel_ClearsToNull pins that a caption can be taken back
// off — including through the blank spelling, which [registrylabel.Normalize]
// folds into the same NULL rather than storing an empty string.
func TestIntegration_UpdateLabel_ClearsToNull(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, labelledProvider("aws-eu", "archon-alice", "AWS")); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	for _, tc := range []struct {
		name  string
		label *string
	}{
		{"nil clears", nil},
		{"blank clears", strPtr("   ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Put a caption back first, so each case actually clears something.
			restore := "AWS"
			if _, err := UpdateLabel(ctx, integrationPool, "aws-eu", &restore); err != nil {
				t.Fatalf("UpdateLabel(restore): %v", err)
			}
			if _, err := UpdateLabel(ctx, integrationPool, "aws-eu", tc.label); err != nil {
				t.Fatalf("UpdateLabel(clear): %v", err)
			}
			got, err := SelectByID(ctx, integrationPool, "aws-eu")
			if err != nil {
				t.Fatalf("SelectByID: %v", err)
			}
			if got.Label != nil {
				t.Errorf("Label = %q after clearing, want nil — an empty string would give "+
					"consumers two ways to spell \"no caption\"", *got.Label)
			}
		})
	}
}

// TestIntegration_UpdateLabel_NotFound pins the sentinel a handler maps to 404:
// addressing a row that does not exist must not silently succeed.
func TestIntegration_UpdateLabel_NotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	label := "whatever"
	_, err := UpdateLabel(ctx, integrationPool, "no-such-provider", &label)
	if err == nil {
		t.Fatal("UpdateLabel on a missing row returned nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("UpdateLabel error = %v, want the not-found sentinel", err)
	}
}

// TestIntegration_Label_IsNotUnique pins the absence of a constraint that would
// be easy to add by reflex: two providers may carry the same caption, because a
// caption is not an address and uniqueness belongs to the thing that is one.
func TestIntegration_Label_IsNotUnique(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	const shared = "Production"
	if err := Insert(ctx, integrationPool, labelledProvider("aws-eu", "archon-alice", shared)); err != nil {
		t.Fatalf("Insert(first): %v", err)
	}
	if err := Insert(ctx, integrationPool, labelledProvider("aws-us", "archon-alice", shared)); err != nil {
		t.Fatalf("Insert(second) with a duplicate caption: %v\n"+
			"ADR-0085: label is deliberately NOT unique — two incarnations may both be "+
			"captioned the same, because a caption is not an address.", err)
	}
}

func strPtr(s string) *string { return &s }
