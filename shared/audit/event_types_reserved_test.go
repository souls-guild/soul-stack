package audit

import "testing"

// retiredEventIDs — wire value → the identifier that owns it, for events whose
// emitter is gone but whose rows are not. The value is spelled as a literal on
// purpose: keyed by the constant, this table would follow a change to the
// constant and assert nothing.
var retiredEventIDs = map[string]string{
	"cloud.provisioned": "EventCloudProvisioned", // NIM-761, core.cloud and the CloudDriver contract
	"provider.created":  "EventProviderCreated",  // NIM-761, the providers registry
	"provider.deleted":  "EventProviderDeleted",
	"profile.created":   "EventProfileCreated",
	"profile.deleted":   "EventProfileDeleted",

	"bootstrap.delivered": "EventBootstrapDelivered", // NIM-834, core.bootstrap.delivered
}

// TestRetiredEventIDsAreNotReused is the reservation rule, in the shape proto
// reserves a field number: a retired event id is never handed to a different
// event.
//
// `audit_log` outlives the code that wrote it. The catalog is what a reader
// labels a stored row by, so pointing a live event at a retired id does not
// merely rename something — it relabels history written under the old
// semantics, and nothing in the row itself contradicts the new label. Dropping
// the constant is the same failure one step removed: the row is then described
// by nothing at all.
//
// Both halves are needed. Presence alone would pass if somebody kept the
// identifier and changed its value; the identifier check alone would pass if
// somebody kept the value and moved it onto a new event. Duplicate values are
// already refused by parseEventTypeDecls, which is what makes "declared once"
// enough here.
//
// Reading the DECLARATIONS rather than [AllEventTypes] catches a violation
// before the catalog is regenerated.
func TestRetiredEventIDsAreNotReused(t *testing.T) {
	decls, err := parseEventTypeDecls(eventTypesSource)
	if err != nil {
		t.Fatalf("reading the event-type declarations: %v", err)
	}

	owner := make(map[string]string, len(decls))
	for _, d := range decls {
		owner[d.value] = d.name
	}

	for value, want := range retiredEventIDs {
		got, ok := owner[value]
		if !ok {
			t.Errorf("retired event id %q is no longer declared: the rows written under it are still in audit_log, and a reader would have nothing to label them with", value)
			continue
		}
		if got != want {
			t.Errorf("retired event id %q is now declared by %s, want %s: handing a reserved id to another event relabels the history already written under it", value, got, want)
		}
	}
}
