package config

import (
	"reflect"
	"testing"
)

// TestRegisterHosts_NameIsReserved — ★ GUARD: `register: hosts` is refused at PARSE.
// The keeper-side context injects the per-host root under exactly that field
// (shared/cel Vars.registerRoot, NIM-711), so such a register is unreadable from
// either side: the accessor wins on the keeper, and on a host `register.hosts` is a
// compile-time isolation error regardless. Both failures land far from the line that
// chose the name; this one lands on it.
//
// Mutation: drop the registerHostsAccessor branch in [validateRegisterField].
func TestRegisterHosts_NameIsReserved(t *testing.T) {
	src := `name: probe
tasks:
  - name: list the nodes
    module: core.exec.run
    register: hosts
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	d := diagWithCode(diags, "register_name_reserved")
	if d == nil {
		dump(t, diags)
		t.Fatalf("expected register_name_reserved for `register: hosts`")
	}
	if d.YAMLPath != "$.tasks[0].register" {
		t.Errorf("YAMLPath = %q, want $.tasks[0].register", d.YAMLPath)
	}
	if d.Line == 0 || d.Column == 0 {
		t.Errorf("diagnostic must carry a position, got %d:%d", d.Line, d.Column)
	}
}

// TestRegisterHosts_NameIsReservedInDestiny — register and id live in ONE address
// space across scenario AND destiny (destiny/tasks.md §8), and a destiny task's
// register reaches the same keeper-side render. The reservation is enforced in
// [validateTaskNode], which both loaders share, so it must hold on both.
//
// Mutation: move the check out of validateTaskNode into the scenario-only path.
func TestRegisterHosts_NameIsReservedInDestiny(t *testing.T) {
	src := `- name: list the nodes
  module: core.exec.run
  register: hosts
  params: { cmd: "true" }
`
	_, diags, _ := LoadDestinyTasksFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "register_name_reserved") {
		dump(t, diags)
		t.Fatalf("expected register_name_reserved on a destiny task")
	}
}

// TestRegisterHosts_ReservationDoesNotCatchNeighbours — the reservation is the exact
// string, not a prefix or a substring: `hosts_seen`/`my_hosts` are ordinary names.
//
// Mutation: compare with strings.Contains in [validateRegisterField].
func TestRegisterHosts_ReservationDoesNotCatchNeighbours(t *testing.T) {
	src := `name: probe
tasks:
  - name: a
    module: core.exec.run
    register: hosts_seen
    params: { cmd: "true" }
  - name: b
    module: core.exec.run
    register: my_hosts
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "register_name_reserved") {
		dump(t, diags)
		t.Fatalf("register_name_reserved must not fire on hosts_seen / my_hosts")
	}
}

// TestExtractRegisterRefs_HostsAccessorNamesTheInnerRegister — ★ GUARD: the accessor
// contributes a dependency on the register BEHIND it, not on the literal `hosts`.
//
// Two failures ride on this one hop, and the louder one is not the dangerous one.
// The loud one: `unknown_register_reference` for a register named "hosts" that no
// task declares. The silent one: [ADR-056] stratification reuses this same parser to
// build the register-dependency graph, so without the hop the capture records NO
// edge to the producing task — it lands in the same Passage as the probe, runs
// before any host has registered anything, and reads an empty map. soul-lint still
// exits 0.
//
// Mutation: drop the `(hosts\.)?` group from reRegisterCELRef.
func TestExtractRegisterRefs_HostsAccessorNamesTheInnerRegister(t *testing.T) {
	got := ExtractRegisterRefs("${ register.hosts.node_id }")
	if !reflect.DeepEqual(got, []string{"node_id"}) {
		t.Fatalf("ExtractRegisterRefs = %v, want [node_id]", got)
	}
}

// TestExtractRegisterRefs_HostsAlone — `register.hosts` with nothing after it names
// no register, so it is no dependency. It cannot be a register name either: an
// author's `register: hosts` is refused at parse (register_name_reserved), so
// treating it as one would only ever produce a phantom unknown_register_reference.
//
// Mutation: drop the `m[2] == "" && name == registerHostsAccessor` clause.
func TestExtractRegisterRefs_HostsAlone(t *testing.T) {
	for _, expr := range []string{
		"register.hosts",
		"has(register.hosts)",
	} {
		if got := ExtractRegisterRefs(expr); len(got) != 0 {
			t.Errorf("ExtractRegisterRefs(%q) = %v, want none", expr, got)
		}
	}
}

// TestExtractRegisterRefs_PayloadFieldNamedHosts — `register.probe.hosts` is a
// payload field that happens to be called hosts; the dependency is on `probe`.
//
// Mutation: widen the hop to any first component (`([a-z][a-z0-9_]*\.)?`), which
// makes the regex capture the LAST of two components instead of the first.
func TestExtractRegisterRefs_PayloadFieldNamedHosts(t *testing.T) {
	got := ExtractRegisterRefs("${ register.probe.hosts }")
	if !reflect.DeepEqual(got, []string{"probe"}) {
		t.Fatalf("ExtractRegisterRefs = %v, want [probe]", got)
	}
}

// TestExtractRegisterRefs_SelfStillFiltered — the accessor hop is additive: the
// pre-existing `register.self` exclusion (same task, not a cross-task edge) still
// holds, and a mix of both forms yields exactly the cross-task names.
func TestExtractRegisterRefs_SelfStillFiltered(t *testing.T) {
	got := ExtractRegisterRefs("register.self.rc == 0 && register.hosts.node_id.size() == register.roster.count")
	if !reflect.DeepEqual(got, []string{"node_id", "roster"}) {
		t.Fatalf("ExtractRegisterRefs = %v, want [node_id roster]", got)
	}
}

// TestExtractRegisterRefs_MethodOnTheAccessorIsNotARegister — a name immediately
// followed by `(` AFTER the hosts hop is a method on the accessor, not a register.
// The hop is what makes the shape reachable: before it, `register.hosts.size()`
// yielded the (already phantom) name `hosts`; with the hop it would yield `size` and
// raise unknown_register_reference on an expression that reads no register at all.
//
// ★ The exclusion stops at the hop. `register.size()` on the PLAIN root has always
// extracted `size` here, so soul-lint refuses it (unknown_register_reference) and
// [IsStaticPredicate] stays false — widening the filter to the plain form would drop
// both, making a predicate the Soul used to evaluate into a static one the keeper
// evaluates against an empty register: always false, task silently skipped. This
// ticket does not touch the plain form.
//
// Mutation: widen the clause to `m[4] == "("`, or drop it entirely.
func TestExtractRegisterRefs_MethodOnTheAccessorIsNotARegister(t *testing.T) {
	if got := ExtractRegisterRefs("register.hosts.size() > 0"); len(got) != 0 {
		t.Errorf(`ExtractRegisterRefs("register.hosts.size() > 0") = %v, want none`, got)
	}
	if got := ExtractRegisterRefs("register.size() > 0"); !reflect.DeepEqual(got, []string{"size"}) {
		t.Errorf("ExtractRegisterRefs = %v, want [size] (the plain form is unchanged)", got)
	}
	// The exclusion is one token wide: a method deeper in the path leaves the
	// register itself intact.
	if got := ExtractRegisterRefs("register.hosts.node_id.size() > 0"); !reflect.DeepEqual(got, []string{"node_id"}) {
		t.Errorf("ExtractRegisterRefs = %v, want [node_id]", got)
	}
}
