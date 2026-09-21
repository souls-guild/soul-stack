package artifact

import (
	"encoding/json"
	"strings"
	"testing"
)

// A create scenario that carries `id_template` composes the incarnation id
// server-side and REFUSES a request that also carries an `id`
// (scenario.ErrIDNotComposable → 422). Nothing in the scenario descriptor said
// so, so a client had no way to know: it asked for an id it must not send, and
// the form deadlocked against the backend (NIM-340).
//
// The descriptor now carries a boolean. Deliberately only that — the template
// itself is not published, because the operator is shown the RESULT of the
// composition, never the formula.

func TestListScenarios_ComposesIDFollowsTheTemplate(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", "name: create\ncreate: true\nid_template: \"${ input.cluster }-${ input.shard }\"\ninput:\n  cluster: {type: string}\n  shard: {type: string}\ntasks: []\n")
	// The retired spelling still declares the same thing for the length of the
	// ADR-0085 window — the listing must not tell a form there is nothing to
	// compose just because the file has not been migrated yet.
	writeScenario(t, root, "create_legacy", "name: create_legacy\ncreate: true\nname_template: \"${ input.cluster }\"\ninput:\n  cluster: {type: string}\ntasks: []\n")
	writeScenario(t, root, "create_named", "name: create_named\ncreate: true\ninput:\n  size: {type: int}\ntasks: []\n")

	got, err := ListScenarios(root, nil)
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	by := map[string]Scenario{}
	for _, s := range got {
		by[s.Name] = s
	}

	if !by["create"].ComposesID {
		t.Error("a scenario declaring id_template did not report that it composes the id — the form will keep asking for one the backend refuses")
	}
	if !by["create_legacy"].ComposesID {
		t.Error("a scenario still on name_template did not report that it composes the id — the compatibility window reaches the descriptor too")
	}
	if by["create_named"].ComposesID {
		t.Error("a scenario without a template reported composing one — the form would stop asking for an id that is still required")
	}
}

// The flag travels; the template does not. Checked on the marshalled JSON rather
// than the struct, because that is what the client actually receives.
func TestScenario_JSONCarriesTheFlagAndNotTheTemplate(t *testing.T) {
	root := t.TempDir()
	const secretish = "${ input.cluster }-${ input.shard }"
	writeScenario(t, root, "create", "name: create\ncreate: true\nid_template: \""+secretish+"\"\ninput:\n  cluster: {type: string}\ntasks: []\n")

	got, err := ListScenarios(root, nil)
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"composes_id":true`) {
		t.Errorf("reply does not carry composes_id: %s", body)
	}
	if strings.Contains(body, "input.cluster }-") || strings.Contains(body, "id_template") {
		t.Errorf("the template leaked into the reply; the operator is shown the composed ID, never the formula: %s", body)
	}
}

// Absent means absent: a service full of ordinary scenarios must serialize
// exactly as it did before this field existed, or every existing client sees a
// diff it has to reason about.
func TestScenario_FlagIsOmittedWhenFalse(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "restart", "name: restart\ninput: {}\ntasks: []\n")

	got, err := ListScenarios(root, nil)
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "composes_id") {
		t.Errorf("a scenario that composes nothing still carries the key: %s", raw)
	}
}
