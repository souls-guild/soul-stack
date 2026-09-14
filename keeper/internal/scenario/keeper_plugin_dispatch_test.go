package scenario

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakePluginRegistry is a KeeperPluginRegistry over two maps, mirroring
// pluginhost.KeeperSideModules: `keeperSide` is what may be executed, `sides` is
// every module the keeper knows about — including the Soul-side ones, which
// exist here only so a refusal can name them.
type fakePluginRegistry struct {
	keeperSide map[string]module.SoulModule
	sides      map[string]schema.Side
}

func (r fakePluginRegistry) LookupKeeperSide(base string) (module.SoulModule, bool) {
	m, ok := r.keeperSide[base]
	return m, ok
}

func (r fakePluginRegistry) DeclaredSide(base string) (schema.Side, bool) {
	s, ok := r.sides[base]
	return s, ok
}

// ACCEPTANCE 1 (NIM-758): a plugin declaring `side: keeper` executes on the
// keeper. Before this, the address resolved against the core registry only and
// every such step died `unknown keeper-side module` — which is what blocks
// NIM-760/761 and the live redis provision.
//
// Mutation: drop the r.lookupKeeperPlugin fallback in runKeeperTask and the
// module is never reached (failed, "unknown keeper-side module").
func TestApplyKeeperTask_PluginWithSideKeeperExecutes(t *testing.T) {
	mod := &fakeKeeperModule{final: &pluginv1.ApplyEvent{
		Changed: true,
		Output:  mustStruct(t, map[string]any{"vm_id": "i-42"}),
	}}
	r := &Runner{
		keeperModules: fakeKeeperRegistry{},
		keeperPlugins: fakePluginRegistry{
			keeperSide: map[string]module.SoulModule{"democloud.vm": mod},
			sides:      map[string]schema.Side{"democloud.vm": schema.SideKeeper},
		},
	}

	rt := &render.RenderedTask{Index: 0, Module: "democloud.vm.created",
		Params: mustStruct(t, map[string]any{"count": float64(1)})}
	changed, failed, output, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil, rt, nil)
	if failed {
		t.Fatalf("failed=true (%s), want the plugin to have executed keeper-side", msg)
	}
	if !changed {
		t.Error("changed=false, want the plugin's own final event to decide")
	}
	if mod.gotState != "created" {
		t.Errorf("plugin got state %q, want created — the state suffix of the address, as for a core module", mod.gotState)
	}
	if output["vm_id"] != "i-42" {
		t.Errorf("output = %v, want the plugin's output threaded back for register:", output)
	}
}

// ACCEPTANCE 2 (NIM-758): a plugin that did NOT declare `side: keeper` is
// refused, in words that say why, and is not executed.
//
// The silent alternative is the failure this guards: such a step must not be
// answered with "unknown module" (the module exists — the author would go
// looking for a typo that is not there), and must not quietly leave for a host
// (nothing would run it there either).
func TestApplyKeeperTask_PluginWithoutSideKeeperRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		side schema.Side
	}{
		{"explicit soul", schema.SideSoul},
		{"absent key", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{
				keeperModules: fakeKeeperRegistry{},
				keeperPlugins: fakePluginRegistry{
					// Registered, but NOT executable: exactly what the real registry
					// does with a module that declares the Soul side.
					keeperSide: map[string]module.SoulModule{},
					sides:      map[string]schema.Side{"redis.acl": tc.side},
				},
			}

			rt := &render.RenderedTask{Index: 1, Module: "redis.acl.present"}
			_, failed, _, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil, rt, nil)
			if !failed {
				t.Fatal("failed=false, want a refusal — a Soul-side module must not run on the keeper")
			}
			// "not executed" is guarded against REAL code in
			// pluginhost.TestKeeperSideModules_IndexesOnlyKeeperSide: the registry
			// there is the production one, and it is what makes a Soul-side module
			// unreachable. Asserting it here would only prove this fake was built
			// the way this test built it, so no module is even constructed.
			if strings.Contains(msg, "unknown keeper-side module") {
				t.Errorf("message = %q, want it to say the module runs on the Soul, not that it does not exist", msg)
			}
			if !strings.Contains(msg, "redis.acl") || !strings.Contains(msg, "side") {
				t.Errorf("message = %q, want it to name the module and its side", msg)
			}
		})
	}
}

// ACCEPTANCE 4 / NIM-688: an address nothing declares still gets the same fast,
// readable refusal it always got — not a plugin-spawn timeout, and not a
// different sentence depending on whether a plugin registry happens to be wired.
func TestApplyKeeperTask_UnknownPluginAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		reg  KeeperPluginRegistry
	}{
		{"no plugin registry", nil},
		{"registry without the address", fakePluginRegistry{
			keeperSide: map[string]module.SoulModule{},
			sides:      map[string]schema.Side{"other.thing": schema.SideKeeper},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{keeperModules: fakeKeeperRegistry{}, keeperPlugins: tc.reg}
			_, failed, _, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil,
				&render.RenderedTask{Module: "democloud.vm.created"}, nil)
			if !failed {
				t.Fatal("failed=false, want true")
			}
			if !strings.Contains(msg, `unknown keeper-side module "democloud.vm.created"`) {
				t.Errorf("message = %q, want the unchanged unknown-module refusal", msg)
			}
		})
	}
}

// A core address keeps resolving against the core registry even when a plugin
// registry is wired under the same name: core is asked first, so nothing an
// operator installs can shadow the module that really runs.
func TestApplyKeeperTask_CoreWinsOverPlugin(t *testing.T) {
	core := &fakeKeeperModule{final: &pluginv1.ApplyEvent{Changed: true, Message: "core"}}
	plug := &fakeKeeperModule{final: &pluginv1.ApplyEvent{Changed: true, Message: "plugin"}}
	r := &Runner{
		keeperModules: fakeKeeperRegistry{"core.soul": core},
		keeperPlugins: fakePluginRegistry{
			keeperSide: map[string]module.SoulModule{"core.soul": plug},
			sides:      map[string]schema.Side{"core.soul": schema.SideKeeper},
		},
	}
	_, failed, _, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil,
		&render.RenderedTask{Module: "core.soul.registered"}, nil)
	if failed || msg != "core" {
		t.Fatalf("failed=%v msg=%q, want the built-in core module to have run", failed, msg)
	}
	if plug.gotState != "" {
		t.Error("the plugin ran — a plugin registered under a core address must not shadow it")
	}
}

// ACCEPTANCE 3 (NIM-758): a secret arriving in a keeper-side plugin's PARAMS is
// masked on the way out.
//
// A plugin has no Vault access of its own and is given its credentials as
// params (NIM-757, decided 2026-09-01). The module then writes the message —
// and a module that echoes what it was handed turns the one channel it controls
// into a leak, into the audit task.executed payload, apply_runs.error_summary
// and the operator's view of both. Neither write-path masker catches it: a
// resolved credential quoted mid-sentence has no sensitive key name and is not
// a `vault:` ref.
//
// Mutation, in the form of real code: delete the maskKeeperTaskMessage call in
// applyKeeperTask (leaving it as the plain return of runKeeperTask) and this
// reddens on the plaintext.
func TestApplyKeeperTask_SecretParamMaskedInMessage(t *testing.T) {
	const secret = "s3cr3t-token-value"
	mod := &fakeKeeperModule{final: &pluginv1.ApplyEvent{
		Failed:  true,
		Message: "authenticate to democloud with token=" + secret + " failed",
	}}
	r := &Runner{
		keeperModules: fakeKeeperRegistry{},
		keeperPlugins: fakePluginRegistry{
			keeperSide: map[string]module.SoulModule{"democloud.vm": mod},
			sides:      map[string]schema.Side{"democloud.vm": schema.SideKeeper},
		},
	}

	rt := &render.RenderedTask{
		Index:  2,
		Module: "democloud.vm.created",
		Params: mustStruct(t, map[string]any{
			"credentials": map[string]any{"token": secret},
			"region":      "ru-central1",
		}),
	}
	// What render recorded for this run: the `credentials.token` cell read a
	// secret source ([ADR-010] §7.4). The path spelling is collectSealed's.
	sealed := map[string]bool{"credentials.token": true}

	_, failed, _, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil, rt, sealed)
	if !failed {
		t.Fatal("failed=false, want the plugin's failure")
	}
	if strings.Contains(msg, secret) {
		t.Fatalf("the secret param leaked into the task message: %q", msg)
	}
	if !strings.Contains(msg, audit.MaskedValue) {
		t.Errorf("message = %q, want the value replaced by %s rather than dropped — the reader still learns a value was there",
			msg, audit.MaskedValue)
	}
	// Everything else the module said survives: masking the value, not the
	// diagnostic, is the whole point of the per-cell seal.
	if !strings.Contains(msg, "authenticate to democloud") {
		t.Errorf("message = %q, want the module's diagnostic intact around the masked value", msg)
	}
	// The observable summary is composed from this message — it must inherit
	// the masking rather than re-derive it.
	if summary := composeKeeperFailure(rt, msg); strings.Contains(summary, secret) {
		t.Errorf("apply_runs.error_summary = %q, want no plaintext secret", summary)
	}
}

// A non-secret param is NOT masked: over-masking a diagnostic is how an
// operator loses the reason a step failed, and the seal is what tells the two
// apart.
func TestMaskKeeperTaskMessage_LeavesUnsealedValues(t *testing.T) {
	rt := &render.RenderedTask{
		Module: "democloud.vm.created",
		Params: mustStruct(t, map[string]any{"region": "ru-central1", "token": "abc"}),
	}
	got := maskKeeperTaskMessage(rt, map[string]bool{"token": true}, "region ru-central1 is closed (token abc)")
	if !strings.Contains(got, "ru-central1") {
		t.Errorf("masked = %q, want the unsealed region left readable", got)
	}
	if strings.Contains(got, "abc") {
		t.Errorf("masked = %q, want the sealed token replaced", got)
	}
}

// A sealed value nested in a list is reached too — the seal's path form is
// dot/idx, and a credential list is an ordinary shape for a provisioning step.
func TestMaskKeeperTaskMessage_ReachesListElements(t *testing.T) {
	rt := &render.RenderedTask{
		Module: "democloud.vm.created",
		Params: mustStruct(t, map[string]any{"keys": []any{"public-part", "private-part"}}),
	}
	got := maskKeeperTaskMessage(rt, map[string]bool{"keys[1]": true}, "rejected key private-part")
	if strings.Contains(got, "private-part") {
		t.Errorf("masked = %q, want the sealed list element replaced", got)
	}
	if !strings.Contains(got, "rejected key") {
		t.Errorf("masked = %q, want the surrounding text intact", got)
	}
}

// One secret being a substring of another must not leave the longer one's tail
// in the clear — the ordering inside the masker is what prevents it.
func TestMaskKeeperTaskMessage_OverlappingSecrets(t *testing.T) {
	rt := &render.RenderedTask{
		Module: "democloud.vm.created",
		Params: mustStruct(t, map[string]any{"short": "abc", "long": "abcdef"}),
	}
	got := maskKeeperTaskMessage(rt, map[string]bool{"short": true, "long": true}, "token abcdef rejected")
	if strings.Contains(got, "def") {
		t.Errorf("masked = %q, want no tail of the longer secret left behind", got)
	}
}

// ACCEPTANCE 5 (NIM-758, the boundary from NIM-747/749 is untouched):
// `on: keeper` on a PLUGIN address still routes the task keeper-side. It is the
// only spelling those scenarios have — a plugin address carries no side the
// scenario file can read — and `services/redis/scenario/provision.yml` is
// written that way today.
func TestPluginAddressWithOnKeeperStillRoutesKeeperSide(t *testing.T) {
	task := config.Task{
		Module: &config.ModuleTask{Module: "democloud.vm.created"},
		On:     config.KeeperTarget,
	}
	if !config.IsKeeperSideTask(task) {
		t.Fatal("a plugin address with `on: keeper` no longer routes keeper-side — the scenarios that depend on it would silently go to hosts")
	}
	// Without the key it is a Soul-side task, as before: the plugin's own side
	// declaration lives in its schema document, which the scenario cannot see.
	if config.IsKeeperSideTask(config.Task{Module: &config.ModuleTask{Module: "democloud.vm.created"}}) {
		t.Error("a plugin address without `on: keeper` routed keeper-side — the scenario has nothing to derive that from")
	}
}

// GUARD (NIM-758): the run's seal actually REACHES the keeper dispatcher.
//
// Every other masking test hands maskKeeperTaskMessage a literal path set, so
// all of them stay green if run.go passes `nil` — and the masker returns its
// input unchanged on an empty set, silently. That is the one edit that turns
// this whole path back into a leak without a single red test, so it is pinned
// at the call sites themselves.
//
// An AST guard rather than a run: reaching dispatchKeeperTasks for real needs
// Postgres, a rendered plan and a service artifact, and the thing being asserted
// is one argument position.
//
// It scans the WHOLE package, not run.go, deliberately. Both call sites live in
// run.go today, and a guard that only reads that file would go on passing while a
// new dispatch path in dispatch.go or claim.go called the same method with `nil` —
// which is precisely the edit it exists to catch.
func TestDispatchKeeperTasks_CallSitesPassTheRunSeal(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	var seen int
	for _, pkg := range pkgs {
		ast.Inspect(pkg, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "dispatchKeeperTasks" {
				return true
			}
			seen++
			if len(call.Args) == 0 {
				t.Errorf("%s: dispatchKeeperTasks called with no arguments", fset.Position(call.Pos()))
				return true
			}
			last := call.Args[len(call.Args)-1]
			got := types.ExprString(last)
			if got != "sealed.Paths()" {
				t.Errorf("%s: keeper dispatch is passed %q as its seal, want sealed.Paths() — a run whose secrets are not threaded here masks nothing, silently",
					fset.Position(call.Pos()), got)
			}
			return true
		})
	}

	// Two call sites: the non-staged/Acolyte path (Passage 0) and the stage loop.
	// A third is not forbidden — it must carry the seal like the other two, which
	// the loop above already checks — but it must not appear unnoticed, and zero
	// means this guard has stopped guarding anything at all.
	if seen != 2 {
		t.Fatalf("found %d dispatchKeeperTasks call sites in the package, want 2 — a new one must carry the seal too", seen)
	}
}
