package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// [ADR-0083] §8 declares a module OUTPUT field secret. That declaration has to reach
// the seal, not only redactSecretOutput: the task's own event is one observable copy
// of the value, and a later cell reading `${ register.<name>.<field> }` is another —
// it is persisted to apply_run_plan.params and served from the /tasks endpoint. The
// `no_log:` this replaces covered both, bluntly. Losing the second half would make
// the retirement a net loss on exactly the module that ships with a secret output.
func TestRender_DeclaredSecretOutputSealsItsRegister(t *testing.T) {
	p := NewPipeline(regKV(), newEngine(t), nil, nil)

	in := RenderInput{
		Scenario: &config.ScenarioManifest{
			Name: "deploy",
			Tasks: []config.Task{
				{
					// Outside the service's own namespace — the case §7 preserves,
					// and the only one in which this module still runs at all.
					Name:     "Read the shared CA",
					On:       "keeper",
					Register: "ca",
					Module: &config.ModuleTask{
						Module: "core.vault.kv-read",
						Params: map[string]any{"path": "secret/shared-ca/prod"},
					},
				},
				{
					Name: "Write it out",
					Module: &config.ModuleTask{
						Module: "core.file.present",
						Params: map[string]any{
							"path":    "/etc/ssl/ca.pem",
							"content": "${ register.ca.data }",
							"owner":   "${ register.ca.path }",
							"mode":    "${ register.probe.stdout }",
						},
					},
				},
			},
		},
		Incarnation:    IncarnationMeta{ID: "redis-prod", Service: "demo-service-redis"},
		Hosts:          []*topology.HostFacts{host("a.example.com", []string{"redis"}, nil)},
		KeeperRegister: map[string]any{"ca": map[string]any{"data": "CA-PEM", "path": "secret/shared-ca/prod"}},
		RegisterByHost: map[string]map[string]any{
			"a.example.com": {"probe": map[string]any{"stdout": "0644"}},
		},
		Sealed: NewSealedSet(),
	}

	if _, _, err := p.Render(context.Background(), in); err != nil {
		t.Fatalf("Render: %v", err)
	}

	paths := in.Sealed.Paths()
	if !paths["content"] {
		t.Errorf("sealed = %v, want the cell reading the declared-secret register sealed", paths)
	}
	// Whole-cell, so the register's non-secret fields seal too — erring wide is the
	// correct direction, and the assertion pins that this is a choice, not an accident.
	if !paths["owner"] {
		t.Errorf("sealed = %v, want the seal to be by register name (whole-cell masking)", paths)
	}
	if paths["mode"] {
		t.Errorf("sealed = %v, want a register whose module declares no secret output left alone", paths)
	}
}

// The scan reaches into `block:` — a task nested there registers into the same flat
// namespace, so a seal that stopped at the top level would leave it unsealed.
func TestSecretOutputRegisters_ReachesIntoBlocks(t *testing.T) {
	tasks := []config.Task{{
		Name: "grouped",
		Block: &config.BlockTask{Block: []config.Task{{
			Register: "ca",
			Module:   &config.ModuleTask{Module: "core.vault.kv-read"},
		}, {
			Register: "plain",
			Module:   &config.ModuleTask{Module: "core.exec.run"},
		}}},
	}}
	got := secretOutputRegisters(tasks, nil)
	if !got["ca"] {
		t.Errorf("secretOutputRegisters = %v, want the nested kv-read register sealed", got)
	}
	if got["plain"] {
		t.Errorf("secretOutputRegisters = %v, want a module with no secret output left alone", got)
	}
}
