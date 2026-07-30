package artifact

import (
	"encoding/json"
	"strings"
	"testing"
)

// A create scenario that carries `name_template` composes the incarnation name
// server-side and REFUSES a request that also carries `name`
// (scenario.ErrNameNotComposable → 422). Nothing in the scenario descriptor said
// so, so a client had no way to know: it asked for a name it must not send, and
// the form deadlocked against the backend (NIM-340).
//
// The descriptor now carries a boolean. Deliberately only that — the template
// itself is not published, because the operator is shown the RESULT of the
// composition, never the formula.

func TestListScenarios_ComposesNameFollowsTheTemplate(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", "name: create\ncreate: true\nname_template: \"${ input.cluster }-${ input.shard }\"\ninput:\n  cluster: {type: string}\n  shard: {type: string}\ntasks: []\n")
	writeScenario(t, root, "create_named", "name: create_named\ncreate: true\ninput:\n  size: {type: int}\ntasks: []\n")

	got, err := ListScenarios(root, nil)
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	by := map[string]Scenario{}
	for _, s := range got {
		by[s.Name] = s
	}

	if !by["create"].ComposesName {
		t.Error("a scenario declaring name_template did not report that it composes the name — the form will keep asking for one the backend refuses")
	}
	if by["create_named"].ComposesName {
		t.Error("a scenario without name_template reported composing one — the form would stop asking for a name that is still required")
	}
}

// The flag travels; the template does not. Checked on the marshalled JSON rather
// than the struct, because that is what the client actually receives.
func TestScenario_JSONCarriesTheFlagAndNotTheTemplate(t *testing.T) {
	root := t.TempDir()
	const secretish = "${ input.cluster }-${ input.shard }"
	writeScenario(t, root, "create", "name: create\ncreate: true\nname_template: \""+secretish+"\"\ninput:\n  cluster: {type: string}\ntasks: []\n")

	got, err := ListScenarios(root, nil)
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"composes_name":true`) {
		t.Errorf("reply does not carry composes_name: %s", body)
	}
	if strings.Contains(body, "input.cluster }-") || strings.Contains(body, "name_template") {
		t.Errorf("the template leaked into the reply; the operator is shown the composed NAME, never the formula: %s", body)
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
	if strings.Contains(string(raw), "composes_name") {
		t.Errorf("a scenario that composes nothing still carries the key: %s", raw)
	}
}
