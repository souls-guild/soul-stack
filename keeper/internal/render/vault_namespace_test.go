package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The runtime half of the own-namespace fence ([ADR-0083] §7).
//
// The load-time half sees the main file only — an `include:` body is parsed
// inside config.ExpandIncludes, after artifact.LoadScenarioManifestResolved has
// returned. By the time a task list reaches Render it is expanded, so this is
// where a path smuggled through an include is caught, and every dispatch path
// (run / preflight / render_host) goes through it.

func ownNamespaceScenario(param string) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "deploy",
		Tasks: []config.Task{{
			Name: "Write the ACL file",
			Module: &config.ModuleTask{
				Module: "core.file.present",
				Params: map[string]any{"path": "/etc/redis/users.acl", "content": param},
			},
		}},
	}
}

func renderOwnNamespace(t *testing.T, scn *config.ScenarioManifest, service string) error {
	t.Helper()
	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), RenderInput{
		Scenario:    scn,
		Incarnation: IncarnationMeta{ID: "prod", Service: service},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"prod"}, nil)},
	})
	return err
}

// A path under the service's own prefix aborts the render, whichever spelling it
// arrived in — before any Vault read is attempted (the client here is nil, so a
// resolve that got as far as Vault would fail with a different message).
func TestRender_OwnNamespaceVaultFenced(t *testing.T) {
	cases := map[string]string{
		"cel macro":   "${ vault('secret/redis/' + incarnation.name + '/redis_users/app#password') }",
		"vault ref":   "vault:secret/redis/prod/redis_users/app#password",
		"other mount": "${ vault('kv/redis/prod/redis_users/app#password') }",
	}
	for name, param := range cases {
		t.Run(name, func(t *testing.T) {
			err := renderOwnNamespace(t, ownNamespaceScenario(param), "redis")
			if err == nil {
				t.Fatal("Render succeeded on a path in the service's own namespace")
			}
			if !strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
				t.Fatalf("err = %v, want %s", err, config.VaultOwnNamespaceCode)
			}
		})
	}
}

// A task list reaching Render only after include expansion is the case the
// load-time fence structurally cannot see. Simulated the way ExpandIncludes
// leaves it: spliced tasks carrying the include's group id.
func TestRender_OwnNamespaceVaultFencedAfterIncludeExpansion(t *testing.T) {
	scn := ownNamespaceScenario("${ vault('secret/redis/prod/redis_users/app#password') }")
	scn.Tasks[0].IncludeGroupID = 1
	scn.Tasks[0].IncludeWhen = "true"
	err := renderOwnNamespace(t, scn, "redis")
	if err == nil || !strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
		t.Fatalf("err = %v, want %s", err, config.VaultOwnNamespaceCode)
	}
}

// A caller with no incarnation identity (push / trial / unit eval) has no owner to
// check a namespace against, and checks nothing rather than everything — the same
// condition resolveRegisterSecrets applies.
func TestRender_OwnNamespaceVaultNoServiceIdentity(t *testing.T) {
	scn := ownNamespaceScenario("${ vault('secret/redis/prod/redis_users/app#password') }")
	err := renderOwnNamespace(t, scn, "")
	if err != nil && strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
		t.Fatalf("the fence fired without a service identity: %v", err)
	}
}

// The fence is on the namespace, not the mechanism: a read outside the service's
// own prefix survives it and reaches the resolve phase, where the nil client is
// what stops it.
func TestRender_CrossNamespaceVaultSurvivesFence(t *testing.T) {
	scn := ownNamespaceScenario("vault:secret/services/shared/tls#ca")
	err := renderOwnNamespace(t, scn, "redis")
	if err == nil {
		t.Fatal("want the nil-client resolve error, got a clean render")
	}
	if strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
		t.Fatalf("the fence fired on a cross-namespace path: %v", err)
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want the vault-resolve nil-client error", err)
	}
}

// The dynamic escape the load-time scan structurally cannot see: the path is not
// spelled anywhere, it is assembled by CEL. The guard runs on the evaluated
// argument, so the assembly stops mattering.
//
// The stub HOLDS the secret, deliberately: without the guard this render succeeds
// and puts the plaintext in Params, which is what the assertion below rules out.
func ownNamespaceDynamicScenario(path string) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "deploy",
		Tasks: []config.Task{{
			Name: "Write the ACL file",
			Vars: map[string]any{"p": path},
			Module: &config.ModuleTask{
				Module: "core.file.present",
				Params: map[string]any{"path": "/etc/redis/users.acl", "content": "${ vault(vars.p) }"},
			},
		}},
	}
}

func renderDynamicVault(t *testing.T, scn *config.ScenarioManifest, service string) ([]*RenderedTask, error) {
	t.Helper()
	kv := &pipelineStubKV{secrets: map[string]map[string]any{
		"secret/redis/prod/redis_users/app": {"password": "leaked-own-namespace"},
		"secret/services/shared/tls":        {"ca": "shared-ca-pem"},
	}}
	p := NewPipeline(kv, vaultEngine(t, kv), nil, nil)
	tasks, _, err := p.Render(context.Background(), RenderInput{
		Scenario:    scn,
		Incarnation: IncarnationMeta{ID: "prod", Service: service},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"prod"}, nil)},
	})
	return tasks, err
}

func TestRender_OwnNamespaceVaultFencedWhenPathIsDynamic(t *testing.T) {
	scn := ownNamespaceDynamicScenario("secret/redis/prod/redis_users/app#password")
	tasks, err := renderDynamicVault(t, scn, "redis")
	if err == nil {
		t.Fatalf("Render succeeded on a dynamically assembled own-namespace path; params = %v", renderedParams(tasks))
	}
	if !strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
		t.Fatalf("err = %v, want %s", err, config.VaultOwnNamespaceCode)
	}
	if strings.Contains(err.Error(), "leaked-own-namespace") {
		t.Fatalf("the secret VALUE reached the error text: %v", err)
	}
}

// A sibling incarnation is fenced for the same reason as this one: the platform
// owns the whole `<mount>/<service>/` prefix, so reaching sideways is the same
// second-copy failure rather than a lesser one.
func TestRender_SiblingIncarnationNamespaceFenced(t *testing.T) {
	scn := ownNamespaceDynamicScenario("secret/redis/staging/redis_users/app#password")
	_, err := renderDynamicVault(t, scn, "redis")
	if err == nil || !strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
		t.Fatalf("err = %v, want %s", err, config.VaultOwnNamespaceCode)
	}
}

// The negative twin: the guard is on the namespace, not on dynamic assembly. A
// dynamically built path OUTSIDE the prefix still resolves.
func TestRender_CrossNamespaceDynamicVaultResolves(t *testing.T) {
	scn := ownNamespaceDynamicScenario("secret/services/shared/tls#ca")
	tasks, err := renderDynamicVault(t, scn, "redis")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := renderedParams(tasks)["content"]; got != "shared-ca-pem" {
		t.Fatalf("content = %v, want the resolved cross-namespace value", got)
	}
}

func renderedParams(tasks []*RenderedTask) map[string]any {
	if len(tasks) == 0 || tasks[0].Params == nil {
		return nil
	}
	return tasks[0].Params.AsMap()
}

// A `core.state.<verb>` capture is the one surface where a fenced read is not
// merely read but COMMITTED into incarnation.state, so a hole here writes the
// platform's own secret back as plaintext — exactly what `type: secret` exists to
// prevent. [ADR-0084] made the capture an ordinary task, so it now rides the same
// Render fence as every other params cell instead of a second pass of its own;
// this pins that the move did not drop the check.

func stateCaptureDynamicScenario(path string) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "deploy",
		Tasks: []config.Task{{
			Name: "Record the admin password",
			On:   "keeper",
			Vars: map[string]any{"p": path},
			Module: &config.ModuleTask{
				Module: "core.state.set",
				Params: map[string]any{"field": "admin_password", "value": "${ vault(vars.p) }"},
			},
		}},
	}
}

func TestRender_StateCaptureOwnNamespaceVaultFencedWhenPathIsDynamic(t *testing.T) {
	scn := stateCaptureDynamicScenario("secret/redis/prod/redis_users/app#password")
	tasks, err := renderDynamicVault(t, scn, "redis")
	if err == nil {
		t.Fatalf("Render succeeded on an own-namespace path in a capture; params = %v", renderedParams(tasks))
	}
	if !strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
		t.Fatalf("err = %v, want %s", err, config.VaultOwnNamespaceCode)
	}
	if strings.Contains(err.Error(), "leaked-own-namespace") {
		t.Fatalf("the secret VALUE reached the error text: %v", err)
	}
}

// The negative twin: the fence is on the namespace, so a cross-namespace read
// still resolves into the captured value.
func TestRender_StateCaptureCrossNamespaceVaultResolves(t *testing.T) {
	scn := stateCaptureDynamicScenario("secret/services/shared/tls#ca")
	tasks, err := renderDynamicVault(t, scn, "redis")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := renderedParams(tasks)["value"]; got != "shared-ca-pem" {
		t.Fatalf("captured value = %v, want the resolved cross-namespace value", got)
	}
}

// loop.items and loop.when evaluate in loopInvariantVars, a context built
// separately from the per-task one. Built without the pass ctx it carries neither
// the fence nor the memo nor cancellation — vault() there silently falls back to
// context.Background() ([cel.Vars.Ctx]).
func loopVaultScenario(expr string, items any) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "deploy",
		Tasks: []config.Task{{
			Name: "Write the ACL file",
			Loop: &config.LoopSpec{Items: items, As: "u", When: expr},
			Module: &config.ModuleTask{
				Module: "core.file.present",
				Params: map[string]any{"path": "/etc/redis/users.acl", "content": "x"},
			},
		}},
	}
}

func renderLoopVault(t *testing.T, scn *config.ScenarioManifest, path string) ([]*RenderedTask, error) {
	t.Helper()
	kv := &pipelineStubKV{secrets: map[string]map[string]any{
		"secret/redis/prod/redis_users/app": {"password": "leaked-own-namespace"},
		"secret/services/shared/tls":        {"ca": "shared-ca-pem"},
	}}
	p := NewPipeline(kv, vaultEngine(t, kv), nil, nil)
	tasks, _, err := p.Render(context.Background(), RenderInput{
		Scenario:    scn,
		ServiceVars: map[string]any{"p": path},
		Incarnation: IncarnationMeta{ID: "prod", Service: "redis"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"prod"}, nil)},
	})
	return tasks, err
}

func TestRender_LoopVaultFencedWhenPathIsDynamic(t *testing.T) {
	cases := map[string]*config.ScenarioManifest{
		"loop.items": loopVaultScenario("", "${ [vault(vars.p)] }"),
		"loop.when":  loopVaultScenario("vault(vars.p) != ''", []any{"alice"}),
	}
	for name, scn := range cases {
		t.Run(name, func(t *testing.T) {
			tasks, err := renderLoopVault(t, scn, "secret/redis/prod/redis_users/app#password")
			if err == nil {
				t.Fatalf("Render succeeded on an own-namespace path in %s; params = %v", name, renderedParams(tasks))
			}
			if !strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
				t.Fatalf("err = %v, want %s", err, config.VaultOwnNamespaceCode)
			}
			if strings.Contains(err.Error(), "leaked-own-namespace") {
				t.Fatalf("the secret VALUE reached the error text: %v", err)
			}
		})
	}
}

// The negative twin for the loop axis.
func TestRender_LoopCrossNamespaceVaultResolves(t *testing.T) {
	tasks, err := renderLoopVault(t, loopVaultScenario("", "${ [vault(vars.p)] }"), "secret/services/shared/tls#ca")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1 (one loop element)", len(tasks))
	}
}

// Merge time is the LAST place a scenario puts a value into incarnation.state:
// modify/remove predicates and patches are evaluated per element, after the
// render pass has ended. [Pipeline.StateOpEvaluators] is the only way to obtain
// those evaluators, so the fence cannot be left off by forgetting an argument.
//
// The path here is a literal: [ADR-0084] took the run context out of the
// merge-time scope, so `vars.p` is no longer reachable and a dynamic assembly
// would have to be built out of the element bindings. Fencing the literal is what
// keeps the runtime half honest anyway — the load-time scan already rejects a
// spelled-out own-namespace path, and this is the layer under it.
func TestStateOpEvaluators_OwnNamespaceVaultFenced(t *testing.T) {
	kv := &pipelineStubKV{secrets: map[string]map[string]any{
		"secret/redis/prod/redis_users/app": {"password": "leaked-own-namespace"},
		"secret/services/shared/tls":        {"ca": "shared-ca-pem"},
	}}
	p := NewPipeline(kv, vaultEngine(t, kv), nil, nil)
	matchEval, opEval := p.StateOpEvaluators(context.Background(), "redis")

	own := "secret/redis/prod/redis_users/app#password"

	// patch value.
	got, err := opEval("${ vault('"+own+"') }", nil, false)
	if err == nil {
		t.Fatalf("opEval resolved an own-namespace path into a patch value: %v", got)
	}
	if !strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
		t.Fatalf("err = %v, want %s", err, config.VaultOwnNamespaceCode)
	}
	if strings.Contains(err.Error(), "leaked-own-namespace") {
		t.Fatalf("the secret VALUE reached the error text: %v", err)
	}

	// modify/remove match predicate.
	if _, err := opEval("vault('"+own+"') != ''", nil, true); err == nil ||
		!strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
		t.Fatalf("match predicate err = %v, want %s", err, config.VaultOwnNamespaceCode)
	}

	// add-dedup match: the same fence on the other evaluator.
	if _, err := matchEval("vault('"+own+"') != ''", nil, nil); err == nil ||
		!strings.Contains(err.Error(), config.VaultOwnNamespaceCode) {
		t.Fatalf("add match err = %v, want %s", err, config.VaultOwnNamespaceCode)
	}

	// The negative twin: outside the prefix the read still resolves.
	got, err = opEval("${ vault('secret/services/shared/tls#ca') }", nil, false)
	if err != nil {
		t.Fatalf("cross-namespace opEval: %v", err)
	}
	if got != "shared-ca-pem" {
		t.Fatalf("patch value = %v, want the resolved cross-namespace value", got)
	}
}
