package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// contractDestiny mirrors the ticket's target shape (NIM-167): a typed input:
// contract with an enum, a conditionally-required field and cross-field
// invariants in validate:.
func contractDestiny() *ResolvedDestiny {
	return &ResolvedDestiny{
		Name: "pilot-contract",
		Input: config.InputSchemaMap{
			"redis_type":    {Type: "string", Required: true, Enum: []any{"standalone", "cluster"}},
			"port":          {Type: "integer", RequiredWhen: "input.redis_type == 'standalone'"},
			"cluster_nodes": {Type: "array", Items: &config.InputSchema{Type: "string"}},
			"conf_dir":      {Type: "string", Default: "/etc/redis", Pattern: "^/[A-Za-z0-9._/-]+$"},
		},
		Validate: []config.ValidateRule{{
			That:    "input.redis_type != 'cluster' || size(input.cluster_nodes) >= 3",
			Message: "cluster requires at least 3 nodes",
		}},
		Tasks: []config.Task{{
			Name: "Write the config",
			Module: &config.ModuleTask{
				Module: "core.file.present",
				Params: map[string]any{"path": "${ input.conf_dir }/redis.conf", "content": "${ input.redis_type }"},
			},
		}},
	}
}

// renderContract runs the destiny with the given scenario input passed straight
// through apply.input.
func renderContract(t *testing.T, scenarioInput map[string]any, applyInput map[string]any) ([]*RenderedTask, error) {
	t.Helper()
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), RenderInput{
		Scenario:    applyScenario("pilot-contract", applyInput),
		Input:       scenarioInput,
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     &stubDestinyResolver{resolved: contractDestiny()},
	})
	return tasks, err
}

// TestDestinyInputContract_ValidInputPasses — the tightening (NIM-167) must not
// break a destiny whose input honours its own contract; defaults still merge.
func TestDestinyInputContract_ValidInputPasses(t *testing.T) {
	tasks, err := renderContract(t,
		map[string]any{"kind": "standalone", "port": 6379},
		map[string]any{"redis_type": "${ input.kind }", "port": "${ input.port }"},
	)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if got := tasks[0].Params.GetFields()["path"].GetStringValue(); got != "/etc/redis/redis.conf" {
		t.Errorf("path = %q, want /etc/redis/redis.conf (schema default merged in)", got)
	}
}

// TestDestinyInputContract_ValueValidation — values passed by the parent scenario
// are checked against the destiny's schema (type/enum/pattern), which the pilot
// contract check never did. Every message must name the offending field.
func TestDestinyInputContract_ValueValidation(t *testing.T) {
	cases := []struct {
		name          string
		scenarioInput map[string]any
		applyInput    map[string]any
		wantIn        string
	}{
		{
			name:          "enum",
			scenarioInput: map[string]any{"kind": "sentinel"},
			applyInput:    map[string]any{"redis_type": "${ input.kind }"},
			wantIn:        "$.redis_type",
		},
		{
			name:          "type",
			scenarioInput: map[string]any{"kind": "standalone", "port": "6379"},
			applyInput:    map[string]any{"redis_type": "${ input.kind }", "port": "${ input.port }"},
			wantIn:        `$.port = "6379" does not match type "integer"`,
		},
		{
			name:          "pattern",
			scenarioInput: map[string]any{"kind": "standalone", "port": 6379},
			applyInput:    map[string]any{"redis_type": "${ input.kind }", "port": "${ input.port }", "conf_dir": "relative/path"},
			wantIn:        "$.conf_dir",
		},
		{
			name:          "array item type",
			scenarioInput: map[string]any{"kind": "cluster"},
			applyInput:    map[string]any{"redis_type": "${ input.kind }", "cluster_nodes": []any{"a", 7, "c"}},
			wantIn:        "$.cluster_nodes[1]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renderContract(t, tc.scenarioInput, tc.applyInput)
			if err == nil {
				t.Fatalf("Render succeeded, want rejection naming %s", tc.wantIn)
			}
			if !errors.Is(err, ErrDestinyInputInvalid) {
				t.Errorf("err = %v, want ErrDestinyInputInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err = %q, want it to name %s", err, tc.wantIn)
			}
			if !strings.Contains(err.Error(), `destiny "pilot-contract"`) {
				t.Errorf("err = %q, want it to name the destiny", err)
			}
		})
	}
}

// TestDestinyInputContract_RequiredWhenEnforced — `required_when: true` with no
// value is a refusal. Previously the key existed in the destiny schema but the
// render pass never evaluated it (silent pass-through).
func TestDestinyInputContract_RequiredWhenEnforced(t *testing.T) {
	_, err := renderContract(t,
		map[string]any{"kind": "standalone"},
		map[string]any{"redis_type": "${ input.kind }"},
	)
	if err == nil {
		t.Fatal("Render succeeded, want a refusal: required_when on port is true and no value was passed")
	}
	if !errors.Is(err, ErrDestinyInputInvalid) {
		t.Errorf("err = %v, want ErrDestinyInputInvalid", err)
	}
	if !strings.Contains(err.Error(), `input "port" is required`) {
		t.Errorf("err = %q, want it to name the port field", err)
	}
	if !strings.Contains(err.Error(), "required_when") {
		t.Errorf("err = %q, want it to name the rule that made port required", err)
	}
}

// TestDestinyInputContract_RequiredWhenFalseStaysOptional — the conditional must
// not degrade into an unconditional required.
func TestDestinyInputContract_RequiredWhenFalseStaysOptional(t *testing.T) {
	_, err := renderContract(t,
		map[string]any{"kind": "cluster"},
		map[string]any{"redis_type": "${ input.kind }", "cluster_nodes": []any{"a", "b", "c"}},
	)
	if err != nil {
		t.Fatalf("Render: %v (port is only required for standalone)", err)
	}
}

// TestDestinyInputContract_ValidateRuleFailure — the acceptance criterion: a
// violated `validate:` rule rejects the render with the rule's OWN message, not
// a generic render error.
func TestDestinyInputContract_ValidateRuleFailure(t *testing.T) {
	_, err := renderContract(t,
		map[string]any{"kind": "cluster"},
		map[string]any{"redis_type": "${ input.kind }", "cluster_nodes": []any{"a", "b"}},
	)
	if err == nil {
		t.Fatal("Render succeeded, want the validate: rule to reject a 2-node cluster")
	}
	if !errors.Is(err, ErrDestinyValidateFailed) {
		t.Errorf("err = %v, want ErrDestinyValidateFailed", err)
	}
	if !strings.Contains(err.Error(), "cluster requires at least 3 nodes") {
		t.Errorf("err = %q, want the rule's message verbatim", err)
	}
	var fail *config.ValidateRuleFailure
	if !errors.As(err, &fail) {
		t.Fatalf("err = %v, want a wrapped *config.ValidateRuleFailure", err)
	}
	if fail.Index != 0 {
		t.Errorf("failed rule index = %d, want 0", fail.Index)
	}
}

// TestDestinyInputContract_ValidateRuleIsInputOnly — a destiny validate: rule
// sees ONLY its own input, matching the scenario sandbox (ADR-009 isolation).
// Referencing scenario scope is a compile error, not a silent read.
func TestDestinyInputContract_ValidateRuleIsInputOnly(t *testing.T) {
	d := contractDestiny()
	d.Validate = []config.ValidateRule{{That: "register.probe.changed", Message: "leaks scenario scope"}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), RenderInput{
		Scenario:    applyScenario("pilot-contract", map[string]any{"redis_type": "standalone", "port": 6379}),
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     &stubDestinyResolver{resolved: d},
	})
	if err == nil {
		t.Fatal("Render succeeded, want an undeclared-reference error: validate: is input-only")
	}
	if !strings.Contains(err.Error(), "register") {
		t.Errorf("err = %q, want it to point at the undeclared reference", err)
	}
}

// TestDestinyInputContract_NoValidateSectionIsNoOp — a destiny without validate:
// keeps rendering exactly as before (the common case must stay untouched).
func TestDestinyInputContract_NoValidateSectionIsNoOp(t *testing.T) {
	d := contractDestiny()
	d.Validate = nil
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), RenderInput{
		Scenario:    applyScenario("pilot-contract", map[string]any{"redis_type": "cluster", "cluster_nodes": []any{"a"}}),
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     &stubDestinyResolver{resolved: d},
	})
	if err != nil {
		t.Fatalf("Render: %v (a 1-node cluster is fine without a validate: rule)", err)
	}
	if len(tasks) != 1 {
		t.Errorf("len(tasks) = %d, want 1", len(tasks))
	}
}

// TestDestinyInputContract_AssertTaskBoundaryUnchanged pins the assert boundary
// NIM-167 did NOT move: render-time `assert:` is a SCENARIO task (diverted by
// IsAssertTask in the scenario loop); inside a destiny it has never been
// supported — guardDestinyTask rejects it as a non-module task. So for a destiny
// the new validate: block is the only declarative input gate, and it does not
// displace anything that used to work here.
func TestDestinyInputContract_AssertTaskBoundaryUnchanged(t *testing.T) {
	d := contractDestiny()
	d.Tasks = append(d.Tasks, config.Task{
		Name:   "Guard the port range",
		Assert: &config.AssertSpec{That: []string{"input.port > 1024"}, Message: "port must be unprivileged"},
	})
	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), RenderInput{
		Scenario:    applyScenario("pilot-contract", map[string]any{"redis_type": "standalone", "port": 6379}),
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     &stubDestinyResolver{resolved: d},
	})
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (assert: is scenario-only, unchanged by NIM-167)", err)
	}
}
