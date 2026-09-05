package render

import (
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ★ NIM-826 — the FIFTH hop, and the one that is not a binding the scenario
// created. NIM-811/812/822/823 each carried a secret through something the run
// made up (`vars:`, `compute:`, a destiny's input, a `loop:` bind); this one
// reads the incarnation's OWN persisted state, where a property declared
// `secret: true` ([ADR-010] §7.4) keeps its value in plaintext — only
// `type: secret` ([ADR-0083] §1) keeps it in Vault.
//
// Everything renders through the real [Pipeline.Render] for the reason the hop
// tests state: the detector was never the broken part, the set handed to it was.

const stateSecret = "s3cr3t-state-pw"

// stateHopScenario — a scenario with NO secret input at all, so the only secret
// source is the state read itself. A scenario declaring one would let the case
// pass on the input address and say nothing about this fix.
func stateHopScenario(tasks ...config.Task) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "restart",
		Input: config.InputSchemaMap{
			"reason": {Type: "string"},
		},
		Tasks: tasks,
	}
}

// stateHopRenderInput — the run's pre-run state snapshot plus the manifest's
// verdict on which of its fields are secret. The two arrive separately and mean
// different things: `State` is what the run reads, `SecretStateFields` is what
// the service DECLARED, and a field with no value yet must still seal the cell
// that will read it after a capture.
func stateHopRenderInput(scn *config.ScenarioManifest) RenderInput {
	return RenderInput{
		Scenario:    scn,
		Input:       map[string]any{"reason": "config change"},
		Incarnation: IncarnationMeta{ID: "redis-prod", Service: "wb-service-redis"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"redis"}, nil)},
		State: map[string]any{
			"db_password": stateSecret,
			"port":        "6379",
			"tls":         map[string]any{"key": stateSecret, "cert": "-----BEGIN CERTIFICATE-----"},
		},
		// What incarnation.StateSchemaSecretFields derives from a state_schema
		// declaring `db_password: {secret: true}` and `tls: {properties: {key:
		// {secret: true}}}` — the top segment of each collected path.
		SecretStateFields: map[string]bool{"db_password": true, "tls": true},
	}
}

// ★ The ticket's own failure scenario: a `secret: true` state field read into a
// params cell whose KEY says nothing (`cmd`), so the regex last resort does not
// catch it either. The non-secret state field in the same task is the other half
// — without it a fix that seals the whole `incarnation.state` binding passes,
// and it would mask nearly every cell in a real scenario.
func TestRender_SealFollowsIncarnationStateSecret(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := stateHopRenderInput(stateHopScenario(config.Task{
		Name: "connect",
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{
				"cmd":   "redis-cli -a ${ incarnation.state.db_password }",
				"chdir": "/var/lib/redis",
				"port":  "${ incarnation.state.port }",
			},
		},
	}))

	paths, params := renderHop(t, p, in, 0)

	if !paths["cmd"] {
		t.Errorf("the cell reading incarnation.state.db_password is NOT sealed: %v\n"+
			"a `secret: true` state field lives in state as plaintext and lands in apply_run_plan.params", paths)
	}
	if paths["port"] || paths["chdir"] {
		t.Errorf("a non-secret cell was sealed: %v — sealing incarnation.state whole masks every state read there is", paths)
	}

	// What the run-plan write actually sees.
	masked := audit.MaskSecretsSealed(params, audit.SealOpts{Sealed: paths})
	if got := masked["cmd"]; got == params["cmd"] {
		t.Errorf("cmd survived masking as %v — the plaintext lands in apply_run_plan.params", got)
	}
	if got := masked["port"]; got != params["port"] {
		t.Errorf("port was masked as %v — an operator's diagnostic surface is gone", got)
	}
}

// The keeper-side path builds its own incarnation root ([keeperVars]) and
// collects its seal at its own call site, so a fix applied to one of the two
// leaves the other open. The comment on that function claimed state was
// "operator-facts, not secrets" until this ticket.
func TestRender_SealFollowsIncarnationStateOnKeeperTask(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := stateHopRenderInput(stateHopScenario(config.Task{
		Name: "re-stash the credential",
		On:   config.KeeperTarget,
		Module: &config.ModuleTask{
			Module: "core.state.present",
			Params: map[string]any{
				"path":  "auth.password",
				"value": "${ incarnation.state.db_password }",
				"port":  "${ incarnation.state.port }",
			},
		},
	}))

	paths, _ := renderHop(t, p, in, 0)

	if !paths["value"] {
		t.Errorf("a keeper-side cell reading incarnation.state.db_password is NOT sealed: %v", paths)
	}
	if paths["path"] || paths["port"] {
		t.Errorf("a non-secret keeper-side cell was sealed: %v", paths)
	}
}

// A state secret hoisted into a task `vars:` or a run-level `compute:` — the
// NIM-811 shape over the NIM-826 source. `compute:` matters on its own: it
// builds its source set separately from the pass-level one, so an address added
// to only one of the two leaves this hop open.
func TestRender_SealFollowsIncarnationStateThroughVarsAndCompute(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	scn := stateHopScenario(config.Task{
		Name: "write redis.conf",
		Vars: map[string]any{"pw": "${ incarnation.state.db_password }"},
		Module: &config.ModuleTask{
			Module: "core.file.present",
			Params: map[string]any{
				"content": "requirepass ${ vars.pw }",
				"banner":  "listening on ${ compute.endpoint }",
				"auth":    "${ compute.pw }",
			},
		},
	})
	scn.Compute = config.ComputeBlock{
		{Name: "pw", Value: "${ incarnation.state.db_password }"},
		{Name: "endpoint", Value: "127.0.0.1:${ incarnation.state.port }"},
	}

	paths, _ := renderHop(t, p, stateHopRenderInput(scn), 0)

	if !paths["content"] {
		t.Errorf("the cell reading vars.pw is NOT sealed: %v — a state secret routed through vars: loses its masking", paths)
	}
	if !paths["auth"] {
		t.Errorf("the cell reading compute.pw is NOT sealed: %v — sealedComputeNames builds its own source set", paths)
	}
	if paths["banner"] {
		t.Errorf("the cell reading a compute entry over a NON-secret state field was sealed: %v", paths)
	}
}

// A secret nested inside a state field (`tls.key`) is addressable only at its
// top segment, so `tls` seals whole — including the certificate beside the key,
// which is public. The safe direction, and the same subtree rule a resolved
// `vault:` map already follows; stated as a test so the over-seal is a decision
// on the record rather than a surprise in a diagnostic.
func TestRender_IncarnationStateNestedSecretSealsItsTopSegment(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := stateHopRenderInput(stateHopScenario(config.Task{
		Name: "write certs",
		Module: &config.ModuleTask{
			Module: "core.file.present",
			Params: map[string]any{
				"key":  "${ incarnation.state.tls.key }",
				"cert": "${ incarnation.state.tls.cert }",
				"port": "${ incarnation.state.port }",
			},
		},
	}))

	paths, _ := renderHop(t, p, in, 0)

	if !paths["key"] {
		t.Errorf("the cell reading the nested secret incarnation.state.tls.key is NOT sealed: %v", paths)
	}
	if !paths["cert"] {
		t.Errorf("a sibling of the nested secret is NOT sealed: %v — CEL never names the leaf alone,\n"+
			"so the subtree is the addressable unit and leaving it open is the leak", paths)
	}
	if paths["port"] {
		t.Errorf("the subtree rule spread past its own field: %v", paths)
	}
}

// ★ THE TWO HALVES JOINED, off a real manifest rather than a hand-written name
// set. Every other case here writes `SecretStateFields` by hand, which is what
// let the first version of this fix pass its own tests while sealing the wrong
// thing: the derivation said `redis_users` and the fixtures never asked it.
//
// The `redis_users` half is the examples/service/redis shape in structure — a
// `type: secret` under `items:`, keyed by a sibling — trimmed of the properties
// that make no difference here; the corpus carries two such collections. The other two are NOT in the
// corpus and are honest about it: no shipped service declares `secret: true` on a
// state property at all, and none carries a top-level scalar `type: secret`. That
// is worth knowing rather than hiding, because it says what this fix does today —
// on the shipped services it produces no address, and the only thing an incorrect
// version could do was over-seal.
func TestRender_SealFromRealManifestSealsTheFlagAndSparesTheCollection(t *testing.T) {
	var schema config.InputSchemaMap
	if err := yaml.Unmarshal([]byte(`
db_password: { type: string, secret: true }
vault_ref:   { type: secret }
port:        { type: string }
redis_users:
  type: array
  items:
    type: object
    properties:
      name:  { type: string, required: true }
      perms: { type: string, required: true }
      password:
        type: secret
        key: name
`), &schema); err != nil {
		t.Fatalf("state_schema fixture does not parse: %v", err)
	}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := stateHopRenderInput(stateHopScenario(config.Task{
		Name: "reconcile acl",
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{
				"cmd":       "redis-cli -a ${ incarnation.state.db_password }",
				"inventory": "${ incarnation.state.redis_users }",
				"stray":     "${ incarnation.state.vault_ref }",
			},
		},
	}))
	in.State = map[string]any{
		"db_password": stateSecret,
		"port":        "6379",
		// What the record holds after config.StripDeclaredSecrets: the identity and
		// the perms, never the password.
		"redis_users": []any{map[string]any{"name": "alice", "perms": "+@read"}},
		// A `type: secret` path that HAS a value — the shape the merge strips and
		// the upgrade write does not, so a migration can leave one here. Exactly the
		// case the exact-address rule keeps covered.
		"vault_ref": stateSecret,
	}
	// The one line under test: the real derivation, not a literal.
	in.SecretStateFields = incarnation.StateSchemaSecretFields(
		&artifact.ServiceArtifact{Manifest: &config.ServiceManifest{StateSchema: schema}})

	paths, _ := renderHop(t, p, in, 0)

	if !paths["cmd"] {
		t.Errorf("the cell reading the `secret: true` field is NOT sealed: %v", paths)
	}
	if paths["inventory"] {
		t.Errorf("the cell reading `redis_users` IS sealed: %v\n"+
			"Its only secret is a `type: secret` one level down, so the address widens to the whole\n"+
			"collection — this masks the ACL inventory out of the run plan and masks no secret at all.", paths)
	}
	if !paths["stray"] {
		t.Errorf("the cell reading the top-level `type: secret` field is NOT sealed: %v\n"+
			"That address is exact, and the value is only ABSENT on the paths that strip it — the\n"+
			"upgrade write does not, so this is the one shape where the marker still earns an address.", paths)
	}
}

// No declared state secret → no state address at all, and reading state seals
// nothing. Most services are this case, and a seal that fired for them would
// mask an operator's whole diagnostic surface for nothing.
func TestRender_IncarnationStateWithoutADeclaredSecretIsNotSealed(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := stateHopRenderInput(stateHopScenario(config.Task{
		Name: "connect",
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{"cmd": "redis-cli -p ${ incarnation.state.port }"},
		},
	}))
	in.SecretStateFields = nil

	paths, _ := renderHop(t, p, in, 0)

	if len(paths) != 0 {
		t.Errorf("sealed paths = %v, want none — this service declares no state secret", paths)
	}
}
