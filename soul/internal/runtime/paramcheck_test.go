package runtime

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	"github.com/souls-guild/soul-stack/soul/internal/coremod"
	installmod "github.com/souls-guild/soul-stack/soul/internal/coremod/module"
	"github.com/souls-guild/soul-stack/soul/internal/pluginhost"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// coreLayer builds the production-shaped registry: the static core layer with
// its embedded manifest contract, fronted by fakes so no host is touched.
func coreLayer(mods map[string]module.SoulModule) Registry {
	return NewCoreParamSchema(mapRegistry(mods))
}

func recordingModule(called *bool) *fakeModule {
	return &fakeModule{applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
		*called = true
		return stream.Send(&pluginv1.ApplyEvent{Changed: true})
	}}
}

// TestParams_UnknownCoreParamFailsWithoutApply — THE guard of ADR-0076: a param
// the binary's manifest does not declare must fail the task loudly, and the
// module must never run. Before this, `core.pkg.installed` with a param the
// module does not read reported CHANGED with no effect.
func TestParams_UnknownCoreParamFailsWithoutApply(t *testing.T) {
	var applied bool
	r := NewApplyRunner(coreLayer(map[string]module.SoulModule{"core.pkg": recordingModule(&applied)}), nil)
	sink := &recordingSink{}

	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-1",
		Tasks: []*keeperv1.RenderedTask{{
			Name:   "install nginx",
			Module: "core.pkg.installed",
			Params: mustStruct(t, map[string]any{"name": "nginx", "hold_version": true}),
		}},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if applied {
		t.Fatal("module.Apply ran despite an undeclared param - the host was touched")
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED", ev.GetStatus())
	}
	if got := ev.GetError().GetCode(); got != "module.unknown_param" {
		t.Errorf("error code = %q, want module.unknown_param", got)
	}
	// The message must name the offending param, not just complain.
	if msg := ev.GetError().GetMessage(); !strings.Contains(msg, "hold_version") {
		t.Errorf("message does not name the param: %q", msg)
	}
	if sink.runResult.GetStatus() == keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Error("run reported SUCCESS with a rejected task")
	}
}

// TestParams_KnownCoreParamsPass — the regression the strictness must not cause:
// every param a core manifest declares still dispatches.
func TestParams_KnownCoreParamsPass(t *testing.T) {
	cases := []struct {
		module string
		params map[string]any
	}{
		{"core.pkg.installed", map[string]any{"name": "nginx", "version": "1.2.3"}},
		{"core.file.present", map[string]any{"path": "/etc/x", "content": "hi", "mode": "0640", "owner": "root", "group": "root"}},
		{"core.exec.run", map[string]any{"cmd": "true", "args": []any{"-x"}, "cwd": "/tmp", "creates": "/tmp/x"}},
		{"core.service.running", map[string]any{"name": "nginx", "enabled": true, "daemon_reload": true}},
		{"core.file.absent", map[string]any{"path": "/etc/x"}},
	}
	for _, tc := range cases {
		modName, state, _ := config.SplitModuleAddr(tc.module)
		var applied bool
		r := NewApplyRunner(coreLayer(map[string]module.SoulModule{modName: recordingModule(&applied)}), nil)
		sink := &recordingSink{}
		if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
			ApplyId: "a",
			Tasks:   []*keeperv1.RenderedTask{{Name: "t", Module: tc.module, Params: mustStruct(t, tc.params)}},
		}, sink); err != nil {
			t.Fatalf("%s: Run: %v", tc.module, err)
		}
		if !applied {
			t.Errorf("%s (state %s): valid params were rejected: %v", tc.module, state, sink.taskEvents[0].GetError())
		}
	}
}

// TestParams_RenderedFileTransportKeysPass — core.file.rendered is the one state
// whose wire form differs from its author-facing manifest: keeper replaces
// `template:`/`vars:` with `template_content`/`render_context` at render
// (ADR-012(d) A1). A check that only knew the manifest would reject every
// rendered-file task in the estate.
func TestParams_RenderedFileTransportKeysPass(t *testing.T) {
	var applied bool
	r := NewApplyRunner(coreLayer(map[string]module.SoulModule{"core.file": recordingModule(&applied)}), nil)
	sink := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "a",
		Tasks: []*keeperv1.RenderedTask{{
			Name:   "render nginx.conf",
			Module: "core.file.rendered",
			Params: mustStruct(t, map[string]any{
				"path":             "/etc/nginx/nginx.conf",
				"mode":             "0644",
				"template_content": "worker_processes {{ .vars.workers }};",
				"render_context":   map[string]any{"vars": map[string]any{"workers": "4"}},
			}),
		}},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !applied {
		t.Fatalf("rendered-file transport keys were rejected: %v", sink.taskEvents[0].GetError())
	}
}

// TestParams_TransportKeysScopedToTheirState — the carve-out is not a global
// amnesty: template_content on a state that does not own it is still unknown.
func TestParams_TransportKeysScopedToTheirState(t *testing.T) {
	var applied bool
	r := NewApplyRunner(coreLayer(map[string]module.SoulModule{"core.file": recordingModule(&applied)}), nil)
	sink := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "a",
		Tasks: []*keeperv1.RenderedTask{{
			Name:   "t",
			Module: "core.file.present",
			Params: mustStruct(t, map[string]any{"path": "/etc/x", "template_content": "nope"}),
		}},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if applied {
		t.Fatal("template_content was accepted on core.file.present")
	}
	if got := sink.taskEvents[0].GetError().GetCode(); got != "module.unknown_param" {
		t.Errorf("error code = %q, want module.unknown_param", got)
	}
}

// TestParams_ModuleWithoutManifestIsUnchecked — absence of a declaration is not
// a declaration of absence. core.augur ships no embedded manifest, so its params
// must pass rather than all be rejected as unknown.
func TestParams_ModuleWithoutManifestIsUnchecked(t *testing.T) {
	if _, ok := coremanifest.Default().Lookup("core.augur"); ok {
		t.Skip("core.augur gained a manifest - fold it into the enforced set instead")
	}
	var applied bool
	r := NewApplyRunner(coreLayer(map[string]module.SoulModule{"core.augur": recordingModule(&applied)}), nil)
	sink := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "a",
		Tasks: []*keeperv1.RenderedTask{{
			Name:   "probe",
			Module: "core.augur.fetch",
			Params: mustStruct(t, map[string]any{"omen": "vault", "query": "x"}),
		}},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !applied {
		t.Fatalf("a module with no manifest was gated: %v", sink.taskEvents[0].GetError())
	}
}

// TestParams_DryRunRejectsUnknownParam — a dry_run that skipped the check would
// answer "no drift" for a param the module never reads: a false clean, which is
// exactly what ADR-031 forbids.
func TestParams_DryRunRejectsUnknownParam(t *testing.T) {
	r := NewApplyRunner(coreLayer(map[string]module.SoulModule{"core.pkg": &planSafeModule{}}), nil)
	sink := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "a",
		DryRun:  true,
		Tasks: []*keeperv1.RenderedTask{{
			Name:   "t",
			Module: "core.pkg.installed",
			Params: mustStruct(t, map[string]any{"name": "nginx", "hold_version": true}),
		}},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Fatalf("dry_run status = %v, want FAILED (a false clean is worse than a refusal)", ev.GetStatus())
	}
	if got := ev.GetError().GetCode(); got != "module.unknown_param" {
		t.Errorf("error code = %q, want module.unknown_param", got)
	}
}

// pluginLayer builds a runner over one custom module whose manifest declares
// `input` on state `applied`, and reports whether the plugin was ever spawned.
func pluginLayer(t *testing.T, input map[string]sharedplugin.InputParamDef) (*ApplyRunner, *fakeSpawner, *PluginRegistry) {
	t.Helper()
	d := makeDiscovered("acme", "widget")
	d.Manifest.Spec.States = map[string]sharedplugin.StateDef{
		"applied": {Description: "x", Input: input},
	}
	spawner := &fakeSpawner{makeSession: func() *fakeSession {
		return &fakeSession{events: []*pluginv1.ApplyEvent{{Changed: true}}}
	}}
	reg := NewPluginRegistry(spawner, []pluginhost.Discovered{d}, nil)
	return NewApplyRunner(NewCompositeRegistry(coreLayer(nil), reg), nil), spawner, reg
}

func runPluginTask(t *testing.T, r *ApplyRunner, params map[string]any) *recordingSink {
	t.Helper()
	sink := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "a",
		Tasks: []*keeperv1.RenderedTask{{
			Name:   "t",
			Module: "acme.widget.applied",
			Params: mustStruct(t, params),
		}},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return sink
}

// TestParams_UndeclaredPluginParamFails — THE guard of the (t) amendment: a
// custom module's manifest gates exactly like a core one. Before this, an
// undeclared key was logged and the task ran on, so the module quietly did its
// old job while what the author asked for never happened.
func TestParams_UndeclaredPluginParamFails(t *testing.T) {
	r, spawner, reg := pluginLayer(t, map[string]sharedplugin.InputParamDef{"name": {Type: "string"}})
	sink := runPluginTask(t, r, map[string]any{"name": "w", "tls_ca": "PEM"})

	if spawner.spawnCount != 0 {
		t.Fatal("the plugin was spawned despite an undeclared param - the host was touched")
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED", ev.GetStatus())
	}
	if got := ev.GetError().GetCode(); got != "module.unknown_param" {
		t.Errorf("error code = %q, want module.unknown_param", got)
	}
	if msg := ev.GetError().GetMessage(); !strings.Contains(msg, "tls_ca") {
		t.Errorf("message does not name the param: %q", msg)
	}
	// A custom module is upgraded independently of the agent, so "upgrade the
	// soul binary" would send the operator after the wrong artifact.
	if msg := ev.GetError().GetMessage(); strings.Contains(msg, "soul binary") {
		t.Errorf("the message points at the soul binary for a plugin param: %q", msg)
	}
	if _, strictness := reg.StateInput("acme.widget", "applied"); strictness != ParamsEnforced {
		t.Errorf("plugin strictness = %v, want ParamsEnforced", strictness)
	}
}

// TestParams_DeclaredPluginParamsPass — the regression the flip must not cause:
// a manifest that declares what the scenario passes still dispatches.
func TestParams_DeclaredPluginParamsPass(t *testing.T) {
	r, spawner, _ := pluginLayer(t, map[string]sharedplugin.InputParamDef{
		"name":   {Type: "string"},
		"tls":    {Type: "bool"},
		"tls_ca": {Type: "string"},
	})
	sink := runPluginTask(t, r, map[string]any{"name": "w", "tls": true, "tls_ca": "PEM"})

	if spawner.spawnCount != 1 {
		t.Fatalf("a fully declared task did not reach the plugin: %v", sink.taskEvents[0].GetError())
	}
}

// TestParams_DeprecatedPluginParamStillApplies — with the contract enforced,
// `deprecated:` (ADR-0076(r)) is the ONLY way a plugin may shrink it: the param
// keeps working for its whole declared window, and only leaving the manifest at
// removed_in turns the identical task text into unknown_param. A deprecation
// that failed on the spot would be indistinguishable from a removal.
func TestParams_DeprecatedPluginParamStillApplies(t *testing.T) {
	r, spawner, _ := pluginLayer(t, map[string]sharedplugin.InputParamDef{
		"addr": {Type: "string"},
		"address": {Type: "string", Deprecated: &sharedplugin.DeprecatedDef{
			Since: "0.4.0", RemovedIn: "0.6.0", Use: "addr",
		}},
	})
	sink := runPluginTask(t, r, map[string]any{"address": "10.0.0.1"})

	if spawner.spawnCount != 1 {
		t.Fatalf("a deprecated param was rejected instead of warned: %v", sink.taskEvents[0].GetError())
	}
}

// TestParams_CompositeAsksTheServingLayer — core shadows a same-named custom
// module in Lookup, so the schema must come from core too. Otherwise a custom
// manifest would describe params for the static module that actually runs.
func TestParams_CompositeAsksTheServingLayer(t *testing.T) {
	shadow := makeDiscovered("core", "pkg")
	shadow.Manifest.Spec.States = map[string]sharedplugin.StateDef{
		"installed": {Description: "x", Input: map[string]sharedplugin.InputParamDef{"anything": {Type: "string"}}},
	}
	c := NewCompositeRegistry(
		coreLayer(map[string]module.SoulModule{"core.pkg": &fakeModule{}}),
		NewPluginRegistry(&fakeSpawner{}, []pluginhost.Discovered{shadow}, nil),
	)

	in, strictness := c.StateInput("core.pkg", "installed")
	if strictness != ParamsEnforced {
		t.Fatalf("strictness = %v, want ParamsEnforced from the core layer", strictness)
	}
	if _, leaked := in["anything"]; leaked {
		t.Error("the shadowed custom manifest supplied the contract")
	}
	if _, ok := in["name"]; !ok {
		t.Error("core manifest input did not resolve")
	}
}

// TestParams_ProductionRegistryEnforcesEveryManifestedCoreModule — the wiring
// guard, mirroring NIM-161's "the announcement matches the registry that serves
// Lookup": every Soul-side core module that HAS an embedded manifest must
// resolve an enforced contract through the same decorator cmd/soul builds. A
// silent wiring regression would turn the whole gate back off.
func TestParams_ProductionRegistryEnforcesEveryManifestedCoreModule(t *testing.T) {
	reg := coremod.Default(installmod.Deps{})
	schema, ok := NewCoreParamSchema(reg).(ParamSchema)
	if !ok {
		t.Fatal("NewCoreParamSchema does not implement ParamSchema")
	}
	checked := 0
	for _, name := range reg.Names() {
		man, hasManifest := coremanifest.Default().Lookup(name)
		if !hasManifest {
			continue
		}
		for state := range man.Spec.States {
			in, strictness := schema.StateInput(name, state)
			if strictness != ParamsEnforced {
				t.Errorf("%s.%s: strictness = %v, want ParamsEnforced", name, state, strictness)
			}
			if in == nil && len(man.Spec.States[state].Input) > 0 {
				t.Errorf("%s.%s: input did not resolve", name, state)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no core module state was checked - the manifest registry or the module registry is empty")
	}
}

// planSafeModule is a read-safe module for the dry_run path (ADR-031); its Plan
// must never be reached when the params are rejected.
type planSafeModule struct {
	module.BaseModule
	planned bool
}

func (m *planSafeModule) PlanReadSafe() {}

func (m *planSafeModule) Plan(_ *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	m.planned = true
	return stream.Send(&pluginv1.PlanEvent{Changed: false})
}
