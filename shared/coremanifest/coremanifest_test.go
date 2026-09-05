package coremanifest

import (
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// expectedModules is the full set of core declarations after the H2 rollout. Key is
// the top-level name (Namespace+"."+Name), value is the expected states. Guards
// against "forgot to add the module to coreModules" and against state-set regressions.
//
// Keeper-side core (`core.soul`/`core.vault`/`core.choir`) are declared
// via the same mechanism; state names are aligned with the actual dispatch of the
// keeper-side coremods (StateCreated/StateDestroyed, StateRead, present/absent).
var expectedModules = map[string][]string{
	"core.exec":      {"run"},
	"core.file":      {"present", "absent", "rendered"},
	"core.directory": {"present", "absent"},
	"core.pkg":       {"installed", "latest", "absent"},
	"core.service":   {"running", "stopped", "restarted", "enabled", "disabled", "masked"},
	"core.user":      {"present", "absent"},
	"core.group":     {"present", "absent"},
	"core.cmd":       {"shell"},
	"core.cron":      {"present", "absent"},
	"core.mount":     {"present", "absent", "mounted", "unmounted"},
	"core.git":       {"cloned", "pulled"},
	"core.archive":   {"extracted"},
	"core.sysctl":    {"present", "applied"},
	"core.url":       {"fetched"},
	"core.line":      {"present", "absent"},
	"core.repo":      {"present", "absent"},
	"core.firewall":  {"present", "absent"},
	"core.http":      {"probe", "request"},
	"core.noop":      {"run"},                   // no-op/barrier anchor (ADR-015)
	"core.module":    {"installed"},             // SoulModule plugin delivery (ADR-065)
	"core.soul":      {"registered"},            // keeper-side (on: keeper)
	"core.bootstrap": {"issued", "delivered"},   // keeper-side ready-made VM onboarding + delivery
	"core.vault":     {"kv-read", "kv-present"}, // keeper-side (ADR-017): kv-read (explicit read) + kv-present (generate-if-absent)
	"core.choir":     {"present", "absent"},     // keeper-side (ADR-044)
	// keeper-side: the write point of a state field ([ADR-0084]); the state suffix
	// IS the ADR-057 verb, one address per verb.
	"core.state": {"set", "present", "add", "append", "modify", "remove", "unset"},
}

// TestDefault_RegisteredCoreModules — the registry builds (mustBuild does not panic)
// and contains exactly the expected set of core modules with their states.
func TestDefault_RegisteredCoreModules(t *testing.T) {
	reg := Default()
	if got, want := len(reg.Names()), len(expectedModules); got != want {
		t.Errorf("registry has %d modules, expected %d: %v", got, want, reg.Names())
	}
	for name, states := range expectedModules {
		m, ok := reg.Lookup(name)
		if !ok {
			t.Errorf("registry does not contain %q", name)
			continue
		}
		for _, s := range states {
			if _, ok := m.States[s]; !ok {
				t.Errorf("%s: missing state %q", name, s)
			}
		}
		if len(m.States) != len(states) {
			t.Errorf("%s: states = %d, expected %d", name, len(m.States), len(states))
		}
	}
	if _, ok := reg.Lookup("core.nope"); ok {
		t.Error("Lookup of a non-existent module returned ok=true")
	}
}

// TestDefault_RequiredParamsPresent — every required field of every state has a
// non-empty type (the schema validator checks this, but we duplicate it as a
// drift guard: required without type is a declaration bug).
func TestDefault_RequiredParamsPresent(t *testing.T) {
	reg := Default()
	for name := range expectedModules {
		m, _ := reg.Lookup(name)
		for state, def := range m.States {
			for pname, p := range def.Input {
				if p.Required && p.Type == "" {
					t.Errorf("%s.%s: required param %q without type", name, state, pname)
				}
			}
		}
	}
}

// TestNames_Deterministic — R2: Names() returns a sorted order.
func TestNames_Deterministic(t *testing.T) {
	names := Default().Names()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("Names() not sorted: %q after %q", names[i], names[i-1])
		}
	}
}

// TestState_ExecRun — exec.run carries required cmd and optional args/creates/…
func TestState_ExecRun(t *testing.T) {
	def, ok := Default().State("core.exec", "run")
	if !ok {
		t.Fatal("core.exec.run not found")
	}
	cmd, ok := def.Input["cmd"]
	if !ok || !cmd.Required {
		t.Errorf("cmd should be required: %+v", cmd)
	}
	if cmd.Type != "string" {
		t.Errorf("cmd.Type = %q, want string", cmd.Type)
	}
	if args, ok := def.Input["args"]; !ok || args.Required {
		t.Errorf("args should be an optional list: %+v", args)
	}
	// `command` is a common typo for cmd; it must not be in the schema.
	if _, ok := def.Input["command"]; ok {
		t.Error("schema of core.exec.run should not have param 'command'")
	}
}

// TestState_FileStates — present/absent/rendered with the correct required fields.
func TestState_FileStates(t *testing.T) {
	cases := []struct {
		state    string
		required []string
	}{
		{"present", []string{"path"}},
		{"absent", []string{"path"}},
		{"rendered", []string{"path", "template"}},
	}
	for _, tc := range cases {
		def, ok := Default().State("core.file", tc.state)
		if !ok {
			t.Fatalf("core.file.%s not found", tc.state)
		}
		for _, name := range tc.required {
			p, ok := def.Input[name]
			if !ok || !p.Required {
				t.Errorf("core.file.%s: %q should be required, got %+v", tc.state, name, p)
			}
		}
	}
}

// TestState_UnknownState — a missing state yields ok=false.
func TestState_UnknownState(t *testing.T) {
	if _, ok := Default().State("core.exec", "runn"); ok {
		t.Error("non-existent state returned ok=true")
	}
}

// TestRegistrySideMatchesTheCatalog — the declaration surface and the routing
// catalog give ONE answer (NIM-749).
//
// `side` exists on [schema.Module] because a plugin declares its own; a core
// declaration therefore has the field too, and an empty one reads as "soul". Left
// unstamped, `Lookup("core.state").Side` would say soul while
// [IsKeeperSide]("core.state") says keeper — two answers in one package, which is
// the drift this whole ticket is about, reproduced at arm's length.
func TestRegistrySideMatchesTheCatalog(t *testing.T) {
	r := Default()
	for _, addr := range r.Names() {
		m, ok := r.Lookup(addr)
		if !ok {
			t.Fatalf("Names() returned %q which Lookup does not serve", addr)
		}
		if want := SideOf(addr); m.Side != want {
			t.Errorf("%s: declaration Side = %q, catalog says %q", addr, m.Side, want)
		}
	}
	// Anchored, so the loop above cannot pass by comparing two empty strings.
	if m, _ := r.Lookup(StateModuleAddr); m.Side != schema.SideKeeper {
		t.Errorf("%s: Side = %q, want %q", StateModuleAddr, m.Side, schema.SideKeeper)
	}
	if m, _ := r.Lookup(Namespace + ".exec"); m.Side != schema.SideSoul {
		t.Errorf("core.exec: Side = %q, want %q", m.Side, schema.SideSoul)
	}
}
