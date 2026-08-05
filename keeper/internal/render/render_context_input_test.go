package render

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// TestRenderContext_InputRootPresent — Variant B (ADR-010 §3.2 amendment): the
// §3.2 root render_context carries an `input` key (resolved operator input)
// alongside vars/self/role. A template reads `.input.<name>`
// directly, without passthrough via `params.vars`. Also checks back-compat:
// `.vars.*` (author-raised via params.vars) isn't broken — both channels
// coexist.
func TestRenderContext_InputRootPresent(t *testing.T) {
	const tmplPath = "templates/ctx.conf.tmpl"
	const tmplBody = "listen {{ .input.listen }}\n" +
		"level {{ .input.log_level }}\n" +
		"socket {{ .vars.socket }}\n" + // back-compat: explicit params.vars channel is alive
		"family {{ .self.os.family }}\n"

	manifest := &config.ScenarioManifest{
		Name: "ctx-input",
		Tasks: []config.Task{
			{
				Name: "render conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/app/app.conf",
						"template": tmplPath,
						// Only one raised var (back-compat), everything else via .input.
						"vars": map[string]any{"socket": "/run/app.sock"},
					},
				},
			},
		},
	}

	host := &topology.HostFacts{
		SID:       "app-1.example.com",
		Soulprint: map[string]any{"os": map[string]any{"family": "debian"}},
	}
	reader := fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"listen": "0.0.0.0:9100", "log_level": "info"},
		Incarnation: IncarnationMeta{Name: "app-prod"},
		Hosts:       []*topology.HostFacts{host},
		Templates:   reader,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	fields := tasks[0].Params.GetFields()
	rc := fields[paramRenderContext].GetStructValue().AsMap()

	inputSec, ok := rc["input"].(map[string]any)
	if !ok {
		t.Fatalf("render_context.input is missing or not an object: %#v", rc["input"])
	}
	if inputSec["listen"] != "0.0.0.0:9100" || inputSec["log_level"] != "info" {
		t.Errorf("render_context.input is incomplete: %#v", inputSec)
	}
	// back-compat: the vars channel is intact.
	if vs, _ := rc["vars"].(map[string]any); vs["socket"] != "/run/app.sock" {
		t.Errorf("render_context.vars (back-compat) was lost: %#v", rc["vars"])
	}

	// Render with the same engine Soul uses, render_context AS ROOT.
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	out, err := engine.Render(fields[paramTemplateContent].GetStringValue(), rc)
	if err != nil {
		t.Fatalf("soul-render failed (.input.* not available in template?): %v", err)
	}
	for _, want := range []string{"listen 0.0.0.0:9100", "level info", "socket /run/app.sock", "family debian"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in render:\n%s", want, out)
		}
	}
}

// TestRenderContext_BuildRenderContext_EmptyInput — buildRenderContext with
// injectInput=true and nil Input produces an empty map (no panic, strict mode
// on `.input.X` works correctly).
func TestRenderContext_BuildRenderContext_EmptyInput(t *testing.T) {
	rc := buildRenderContext(RenderInput{}, &topology.HostFacts{}, nil, nil, true)
	if _, ok := rc["input"].(map[string]any); !ok {
		t.Fatalf("render_context.input with nil Input must be an empty map, got %#v", rc["input"])
	}
}

// TestRenderContext_BuildRenderContext_InputOmitted — ★conditional injection
// (Variant B): injectInput=false → no `input` key in render_context (templates
// using only `.vars` keep the pre-Variant-B shape {vars,self,role},
// deep-equal fixtures stay stable). vars/self/role are always present; the
// `essence` key is gone with the root (ADR-0082) and must not come back.
func TestRenderContext_BuildRenderContext_InputOmitted(t *testing.T) {
	rc := buildRenderContext(
		RenderInput{Input: map[string]any{"secret": "x"}},
		&topology.HostFacts{}, nil, nil, false)
	if _, present := rc["input"]; present {
		t.Fatalf("with injectInput=false, key input must not be present: %#v", rc)
	}
	for _, k := range []string{"vars", "self", "role"} {
		if _, ok := rc[k]; !ok {
			t.Errorf("render_context.%s must always be present: %#v", k, rc)
		}
	}
	if _, present := rc["essence"]; present {
		t.Errorf("render_context.essence is retired (ADR-0082) — a service's vars reach a template as .vars.*: %#v", rc)
	}
}

// TestRenderContext_WholeVarsReadGetsTheWholeMap — targeted injection is keyed on
// the `.vars.<key>` chains the AST can see. A template that names NONE of them —
// `{{ index .vars "x" }}`, `{{ range $k, $v := .vars }}`, `{{ toYaml .vars }}` —
// would otherwise render an empty map against a map we are holding: a config file
// written with a section missing, no error anywhere, and the service restarted
// onto it. Both spellings ship in examples/ today
// (`redis.conf.tmpl` uses `index .vars "loadmodules"`, mongo's uses `toYaml`).
func TestRenderContext_WholeVarsReadGetsTheWholeMap(t *testing.T) {
	in := RenderInput{ServiceVars: map[string]any{"loadmodules": []any{"a.so"}, "other": 1}}

	for name, content := range map[string]string{
		"index":  `{{ range (index .vars "loadmodules") }}x{{ end }}`,
		"toYaml": `{{ .vars }}`,
		"range":  `{{ range $k, $v := .vars }}{{ $k }}{{ end }}`,
		"with":   `{{ with .vars }}{{ end }}`,
	} {
		t.Run(name, func(t *testing.T) {
			whole, err := templateReadsWholeVars(content)
			if err != nil {
				t.Fatalf("templateReadsWholeVars: %v", err)
			}
			if !whole {
				t.Fatalf("a whole-map read must be detected: %q", content)
			}
			keys, err := templateVarSubKeys(content)
			if err != nil {
				t.Fatalf("templateVarSubKeys: %v", err)
			}
			// The point of the pairing: subkey scoping sees nothing here, so
			// without the whole-map branch the injected map would be empty.
			if scoped := referencedFileVars(in.ServiceVars, keys); len(scoped) != 0 {
				t.Fatalf("expected subkey scoping to find nothing, got %#v", scoped)
			}
		})
	}

	// A named-subkey read stays scoped — the bit-for-bit property is not lost.
	whole, err := templateReadsWholeVars(`{{ .vars.loadmodules }}`)
	if err != nil {
		t.Fatalf("templateReadsWholeVars: %v", err)
	}
	if whole {
		t.Fatal("a named-subkey read must NOT be treated as a whole-map read")
	}
	keys, err := templateVarSubKeys(`{{ .vars.loadmodules }}`)
	if err != nil {
		t.Fatalf("templateVarSubKeys: %v", err)
	}
	scoped := referencedFileVars(in.ServiceVars, keys)
	if len(scoped) != 1 {
		t.Fatalf("scoped injection must carry exactly the named key, got %#v", scoped)
	}
}

// TestRenderContext_VarsOnlyTemplate_NoInput — ★end-to-end conditional
// injection through Render: a template reading ONLY `.vars.*` → render_context
// carries no `input` (even if operator input is present in the run). This
// guarantees redis-like templates (vars-only) don't get a bloated
// render_context and their deep-equal fixtures stay green.
func TestRenderContext_VarsOnlyTemplate_NoInput(t *testing.T) {
	const tmplPath = "templates/vars-only.conf.tmpl"
	// The body mentions `.input` ONLY in a comment (like redis.conf.tmpl) — not an actual reference.
	const tmplBody = "# apply.input resolves host-invariantly, see .input contract\n" +
		"port {{ .vars.port }}\n"

	manifest := &config.ScenarioManifest{
		Name:  "vars-only",
		Input: config.InputSchemaMap{"listen": {Type: "string"}},
		Tasks: []config.Task{
			{
				Name: "render conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/app/app.conf",
						"template": tmplPath,
						"vars":     map[string]any{"port": "6379"},
					},
				},
			},
		},
	}
	host := &topology.HostFacts{SID: "app-1.example.com"}
	reader := fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"listen": "0.0.0.0:9100"},
		Incarnation: IncarnationMeta{Name: "app-prod"},
		Hosts:       []*topology.HostFacts{host},
		Templates:   reader,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	rc := tasks[0].Params.GetFields()[paramRenderContext].GetStructValue().AsMap()
	if _, present := rc["input"]; present {
		t.Fatalf("vars-only template must NOT receive render_context.input: %#v", rc)
	}
	if vs, _ := rc["vars"].(map[string]any); vs["port"] != "6379" {
		t.Errorf("render_context.vars was lost: %#v", rc["vars"])
	}
}

// TestSealS1_SecretInputSealedAndMasked — ★KEY security guard test (ADR-010
// §7.4, mechanism S-1, Variant B). Proves the seal gap is closed: for
// core.file.rendered with a secret:true input field,
//
//	(1) the path render_context.input.<secret> IS MARKED sealed (in.Sealed);
//	(2) the secret value at that path IS MASKED in observable output
//	    (a status_details-like payload), no plaintext leaks.
//
// Without the declarative S-1 (sealRenderContextInput) the path wouldn't be
// sealed — the vars passthrough is gone, so collectSealed over raw params no
// longer sees the secret.
func TestSealS1_SecretInputSealedAndMasked(t *testing.T) {
	const tmplPath = "templates/secret.conf.tmpl"
	const secretVal = "s3cr3t-PLAINTEXT"

	manifest := &config.ScenarioManifest{
		Name: "secret-render",
		Input: config.InputSchemaMap{
			"admin_password": {Type: "string", Secret: true},
			"listen":         {Type: "string"},
		},
		Tasks: []config.Task{
			{
				Name: "render conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/app/app.conf",
						"template": tmplPath,
					},
				},
			},
		},
	}

	host := &topology.HostFacts{SID: "app-1.example.com"}
	reader := fakeReader{files: map[string][]byte{tmplPath: []byte("requirepass {{ .input.admin_password }}\n")}}

	sealed := NewSealedSet()
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"admin_password": secretVal, "listen": "0.0.0.0:9100"},
		Incarnation: IncarnationMeta{Name: "app-prod"},
		Hosts:       []*topology.HostFacts{host},
		Templates:   reader,
		Sealed:      sealed,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// (1) render_context.input.admin_password is marked sealed; the
	// non-secret listen is NOT sealed (no over-seal).
	paths := sealed.Paths()
	const secretPath = "render_context.input.admin_password"
	if !paths[secretPath] {
		t.Fatalf("seal-gap NOT closed: %q not sealed - secret will leak into observable. Paths=%v", secretPath, paths)
	}
	if paths["render_context.input.listen"] {
		t.Errorf("over-seal: non-secret listen marked sealed: %v", paths)
	}

	// The secret genuinely reaches render_context.input (correct — Soul needs
	// the real value, the wire channel isn't masked), which is exactly why
	// observable channels must hide it via the sealed path.
	rc := tasks[0].Params.GetFields()[paramRenderContext].GetStructValue().AsMap()
	if got, _ := rc["input"].(map[string]any); got["admin_password"] != secretVal {
		t.Fatalf("test precondition violated: secret did not reach render_context.input: %#v", got)
	}

	// (2) masking the observable payload via the sealed path: the secret
	// value is replaced, no plaintext remains.
	payload := map[string]any{"render_context": map[string]any{"input": map[string]any{
		"admin_password": secretVal,
		"listen":         "0.0.0.0:9100",
	}}}
	masked := audit.MaskSecretsSealed(payload, audit.SealOpts{Sealed: paths})
	maskedInput := masked["render_context"].(map[string]any)["input"].(map[string]any)
	if maskedInput["admin_password"] == secretVal {
		t.Fatalf("seal-masking did NOT work: plaintext secret %q remained in observable payload", secretVal)
	}
	if maskedInput["listen"] != "0.0.0.0:9100" {
		t.Errorf("non-secret listen incorrectly masked: %#v", maskedInput["listen"])
	}

	// Protection against leaking through free-text error/detail strings: the
	// same sealed set also hides the secret in the string channel at path
	// render_context.input.admin_password.
	strPayload := map[string]any{secretPath: secretVal}
	maskedStr := audit.MaskSecretsSealed(strPayload, audit.SealOpts{Sealed: paths})
	if maskedStr[secretPath] == secretVal {
		t.Errorf("seal-masking by exact path did not work: %v", maskedStr)
	}
}

// TestSealS1_VarsOnlyTemplate_NoSeal — ★conditional seal gate (Variant B): for
// a vars-only template (doesn't read `.input`), render_context.input is not
// injected → the seal path render_context.input.<secret> isn't marked, even
// if the schema has a secret input. This keeps the sealed set in sync with
// render_context's actual shape (no dead sealed paths for a nonexistent
// slot), and the secret physically never reaches render_context.input (it
// isn't there).
func TestSealS1_VarsOnlyTemplate_NoSeal(t *testing.T) {
	const tmplPath = "templates/vars-only-secret.conf.tmpl"
	const tmplBody = "port {{ .vars.port }}\n" // doesn't read .input

	manifest := &config.ScenarioManifest{
		Name: "vars-only-secret",
		Input: config.InputSchemaMap{
			"admin_password": {Type: "string", Secret: true},
		},
		Tasks: []config.Task{
			{
				Name: "render conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/app/app.conf",
						"template": tmplPath,
						"vars":     map[string]any{"port": "6379"},
					},
				},
			},
		},
	}
	host := &topology.HostFacts{SID: "app-1.example.com"}
	reader := fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}}

	sealed := NewSealedSet()
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"admin_password": "s3cr3t"},
		Incarnation: IncarnationMeta{Name: "app-prod"},
		Hosts:       []*topology.HostFacts{host},
		Templates:   reader,
		Sealed:      sealed,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if paths := sealed.Paths(); paths["render_context.input.admin_password"] {
		t.Errorf("vars-only template must not produce a seal-path on input: %v", paths)
	}
	rc := tasks[0].Params.GetFields()[paramRenderContext].GetStructValue().AsMap()
	if _, present := rc["input"]; present {
		t.Fatalf("secret must not reach render_context.input of a vars-only template: %#v", rc)
	}
}

// TestSealS1_VaultScopeInputSealed — a field with vault_scope (the contract
// requires secret:true) also lands in sealed render_context.input.<field>.
// Checks that a scoped-vault secret doesn't leak into observable output.
func TestSealS1_VaultScopeInputSealed(t *testing.T) {
	set := NewSealedSet()
	in := RenderInput{Scenario: &config.ScenarioManifest{Input: config.InputSchemaMap{
		"tls_key": {Type: "string", Secret: true, VaultScope: "secret/app/*"},
		"plain":   {Type: "string"},
	}}}
	sealRenderContextInput(set, in)
	paths := set.Paths()
	if !paths["render_context.input.tls_key"] {
		t.Errorf("vault_scope field tls_key not sealed: %v", paths)
	}
	if paths["render_context.input.plain"] {
		t.Errorf("over-seal of non-secret plain: %v", paths)
	}
}

// TestSealS1_NilSetNoop — nil Sealed (push/trial/Acolyte) → no-op, no panic.
func TestSealS1_NilSetNoop(t *testing.T) {
	sealRenderContextInput(nil, RenderInput{Scenario: &config.ScenarioManifest{
		Input: config.InputSchemaMap{"pw": {Type: "string", Secret: true}},
	}})
}

// TestSecretInputNames_VaultScope — secretInputNames collects both secret:true
// and vault_scope fields (defense-in-depth: the secret name set doesn't rely
// on another validator's invariant that vault_scope ⇒ secret).
func TestSecretInputNames_VaultScope(t *testing.T) {
	scn := &config.ScenarioManifest{Input: config.InputSchemaMap{
		"a": {Type: "string", Secret: true},
		"b": {Type: "string", VaultScope: "secret/x/*"},
		"c": {Type: "string"},
	}}
	got := secretInputNames(scn)
	if !got["a"] || !got["b"] {
		t.Errorf("secret/vault_scope names were not collected: %v", got)
	}
	if got["c"] {
		t.Errorf("non-secret c ended up in the set: %v", got)
	}
}

// TestRenderContext_RootKeySetIsClosed — the root of `render_context` is exactly
// {vars, self, role}, plus `input` only when the template reads it (ADR-0082 §7,
// ADR-010 amendment 2026-06-26).
//
// A closed set, asserted by name, because this root is a wire contract in a
// shape the proto cannot police: it travels as a Struct inside
// RenderedTask.params, so a key added, removed or renamed here moves nothing the
// only-add rule can see. NIM-410 removed `essence` from it — and the fixture
// migration renamed the key in 68 expected blocks instead of deleting it,
// producing `service vars: {}`, a YAML key with a space that no renderer can
// ever emit. Every one of those was an unsatisfiable assertion, and the whole
// example corpus went red; nothing above the corpus noticed, because a case that
// asserts an impossible value still LOADS.
//
// This test is the cheap version of that discovery: it fails the moment the root
// gains or loses a name, without running a single example.
func TestRenderContext_RootKeySetIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		injectInput bool
		want        []string
	}{
		{"template does not read .input", false, []string{"role", "self", "vars"}},
		{"template reads .input", true, []string{"input", "role", "self", "vars"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := buildRenderContext(RenderInput{}, &topology.HostFacts{}, nil, nil, tc.injectInput)
			got := make([]string, 0, len(rc))
			for k := range rc {
				got = append(got, k)
			}
			sort.Strings(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("render_context root keys = %v, want %v — this root is a wire "+
					"contract the proto cannot police; changing it needs an ADR amendment "+
					"and a sweep of every expected block in examples/", got, tc.want)
			}
		})
	}
}
