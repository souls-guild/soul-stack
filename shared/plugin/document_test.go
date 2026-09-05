package plugin

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/diag"
)

func sampleDocumentBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := schema.Marshal(schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Compat:          schema.Compat{Keeper: ">=0.9 <2.0"},
		Modules: []schema.Module{{
			Name:         "acl",
			Description:  "Redis ACL users",
			Capabilities: []schema.Capability{schema.NetworkOutbound},
			SideEffects:  []schema.SideEffect{{User: "redis_acl_user"}},
			States: map[string]schema.State{
				"present": {Description: "The ACL user exists", Input: schema.Input{
					"host": {Type: schema.String, Required: true},
					"port": {Type: schema.Int, Default: 6379},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}

// stampedArtifact writes a fake artifact with a schema trailer and returns its path.
func stampedArtifact(t *testing.T, payload []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "soul-mod-redis")
	body := schema.AppendTrailer([]byte("pretend this is an ELF"), payload)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return path
}

func TestReadArtifact_HappyPath(t *testing.T) {
	path := stampedArtifact(t, sampleDocumentBytes(t))

	doc, diags, err := ReadArtifact(path)
	if err != nil {
		t.Fatalf("ReadArtifact: %v", err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	if doc == nil {
		t.Fatal("no document")
	}
	if doc.Kind != KindSoulModule || doc.ProtocolVersion != 1 {
		t.Fatalf("kind/protocol_version: %q/%d", doc.Kind, doc.ProtocolVersion)
	}
	state, ok := StateOf(doc, "acl", "present")
	if !ok {
		t.Fatal("acl.present is missing")
	}
	if !state.Input["host"].Required {
		t.Fatal("host should be required")
	}
	if _, ok := StateOf(doc, "acl", "absent"); ok {
		t.Fatal("acl.absent should not resolve")
	}
	if _, ok := StateOf(doc, "config", "present"); ok {
		t.Fatal("a module that is not in the document should not resolve")
	}
}

func TestReadArtifact_FailsClosed(t *testing.T) {
	payload := sampleDocumentBytes(t)

	tests := map[string]struct {
		artifact func(t *testing.T) string
		code     string
	}{
		"never_stamped": {
			artifact: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "soul-mod-redis")
				if err := os.WriteFile(path, []byte("a plain artifact"), 0o755); err != nil {
					t.Fatalf("write: %v", err)
				}
				return path
			},
			code: "schema_trailer_missing",
		},
		"magic_damaged": {
			artifact: func(t *testing.T) string {
				path := stampedArtifact(t, payload)
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				body[len(body)-1] = 'X'
				if err := os.WriteFile(path, body, 0o755); err != nil {
					t.Fatalf("write: %v", err)
				}
				return path
			},
			code: "schema_trailer_missing",
		},
		"length_beyond_file": {
			artifact: func(t *testing.T) string {
				path := stampedArtifact(t, payload)
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				binary.BigEndian.PutUint64(body[len(body)-8-len(schema.TrailerMagic):], uint64(len(body))+4096)
				if err := os.WriteFile(path, body, 0o755); err != nil {
					t.Fatalf("write: %v", err)
				}
				return path
			},
			code: "schema_trailer_malformed",
		},
		"absent_file": {
			artifact: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "nothing-here")
			},
			code: "io_error",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := tc.artifact(t)
			doc, diags, err := ReadArtifact(path)
			// Both channels must carry the refusal: a caller checking either one
			// alone still fails closed.
			if err == nil {
				t.Fatal("expected an error")
			}
			if !diag.HasErrors(diags) {
				t.Fatalf("expected an error diagnostic, got %+v", diags)
			}
			if doc != nil {
				t.Fatal("a refused artifact must not yield a document")
			}
			if diags[0].Code != tc.code {
				t.Fatalf("code: got %q want %q", diags[0].Code, tc.code)
			}
			if diags[0].File != path && tc.code != "" {
				t.Fatalf("diagnostic should name the file, got %q", diags[0].File)
			}
		})
	}
}

func TestReadArtifact_DoesNotFallBackToSchemaFile(t *testing.T) {
	// The published schema.json is for soul-lint. A host that cannot read a
	// trailer must refuse the artifact, not read a file anyone could have dropped
	// next to it.
	dir := t.TempDir()
	path := filepath.Join(dir, "soul-mod-redis")
	if err := os.WriteFile(path, []byte("an unstamped artifact"), 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, SchemaFileName), sampleDocumentBytes(t), 0o644); err != nil {
		t.Fatalf("write schema.json: %v", err)
	}
	doc, _, err := ReadArtifact(path)
	if err == nil || doc != nil {
		t.Fatal("ReadArtifact fell back to the sibling schema.json")
	}
}

func TestReadSchemaFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SchemaFileName)
	if err := os.WriteFile(path, sampleDocumentBytes(t), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	doc, diags, err := ReadSchemaFile(path)
	if err != nil || diag.HasErrors(diags) || doc == nil {
		t.Fatalf("ReadSchemaFile: err=%v diags=%+v doc=%v", err, diags, doc)
	}

	_, diags, err = ReadSchemaFile(filepath.Join(dir, "absent.json"))
	if err == nil {
		t.Fatal("expected an I/O error for a missing file")
	}
	if len(diags) != 1 || diags[0].Code != "io_error" {
		t.Fatalf("expected an io_error diagnostic, got %+v", diags)
	}
}

func TestParseDocument_ParseFailureIsFatal(t *testing.T) {
	doc, diags := ParseDocument("schema.json", []byte("panic: the artifact crashed"))
	if doc != nil {
		t.Fatal("a parse failure must not yield a document")
	}
	if len(diags) != 1 || diags[0].Code != "schema_parse_error" || diags[0].Phase != diag.PhaseParse {
		t.Fatalf("diagnostics: %+v", diags)
	}
}

func TestParseDocument_ValidationFailureKeepsTheDocument(t *testing.T) {
	raw, err := schema.Marshal(schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules: []schema.Module{{
			Name: "acl",
			States: map[string]schema.State{
				"present": {Description: "d", Input: schema.Input{"host": {Type: "duration"}}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	doc, diags := ParseDocument("schema.json", raw)
	if doc == nil {
		t.Fatal("a validation failure should still return the parsed document")
	}
	if !diag.HasErrors(diags) {
		t.Fatal("expected validation errors")
	}
	found := false
	for _, d := range diags {
		if d.Code != "input_type_unknown" {
			continue
		}
		found = true
		if d.File != "schema.json" {
			t.Fatalf("file label: %q", d.File)
		}
		if !strings.HasPrefix(d.YAMLPath, "$.modules[acl].states.present.input.host") {
			t.Fatalf("path: %q", d.YAMLPath)
		}
		if d.Phase != diag.PhaseSchemaValidate {
			t.Fatalf("phase: %q", d.Phase)
		}
		if d.Line != 0 || d.Column != 0 {
			t.Fatal("a generated document has no line/column to report")
		}
	}
	if !found {
		t.Fatalf("expected input_type_unknown, got %+v", diags)
	}
}

func TestDiagnostics_MapsLevelAndPhase(t *testing.T) {
	got := Diagnostics("schema.json", []schema.Issue{
		{Level: schema.LevelWarning, Phase: schema.PhaseSchema, Code: "w", Message: "m", Path: "$.a"},
		{Level: schema.LevelError, Phase: schema.PhaseSemantic, Code: "e", Message: "m", Hint: "h", Path: "$.b"},
	})
	if len(got) != 2 {
		t.Fatalf("got %d diagnostics", len(got))
	}
	if got[0].Level != diag.LevelWarning || got[0].Phase != diag.PhaseSchemaValidate {
		t.Fatalf("warning mapping: %+v", got[0])
	}
	if got[1].Level != diag.LevelError || got[1].Phase != diag.PhaseSemanticValidate || got[1].Hint != "h" {
		t.Fatalf("error mapping: %+v", got[1])
	}
	if Diagnostics("schema.json", nil) != nil {
		t.Fatal("no issues should map to no diagnostics")
	}
}

func TestValidateSimple(t *testing.T) {
	doc, _, err := ReadArtifact(stampedArtifact(t, sampleDocumentBytes(t)))
	if err != nil {
		t.Fatalf("ReadArtifact: %v", err)
	}
	if err := ValidateSimple(doc); err != nil {
		t.Fatalf("ValidateSimple: %v", err)
	}
	doc.Modules[0].Capabilities = []Capability{"become_root"}
	err = ValidateSimple(doc)
	if err == nil || !strings.Contains(err.Error(), "capability_unknown") {
		t.Fatalf("ValidateSimple: %v", err)
	}
	if ValidateSimple(nil) == nil {
		t.Fatal("a nil document is not valid")
	}
}

func TestFirstError(t *testing.T) {
	if err := FirstError(nil); err != nil {
		t.Fatalf("FirstError(nil) = %v", err)
	}
	if err := FirstError([]diag.Diagnostic{{Level: diag.LevelWarning, Code: "w", Message: "m"}}); err != nil {
		t.Fatalf("warnings must not become an error: %v", err)
	}
	err := FirstError([]diag.Diagnostic{
		{Level: diag.LevelError, Code: "a", Message: "one"},
		{Level: diag.LevelWarning, Code: "w", Message: "skip"},
		{Level: diag.LevelError, Code: "b", Message: "two"},
	})
	if err == nil || err.Error() != "a: one; b: two" {
		t.Fatalf("FirstError = %v", err)
	}
}

// TestDeprecationMetadataSurvivesTheDocumentFormat guards the surface NIM-205/237/243
// built on: `ScanTasksForDeprecated` in shared/config reads `StateDef.Input[p].Deprecated`
// and stores a `plugin.DeprecatedDef` by value, and the module-catalog UI renders
// `Notice`. The type moved into `sdk/schema` when the manifest became a generated
// document; the names, the fields and the sentence must not have moved with it.
func TestDeprecationMetadataSurvivesTheDocumentFormat(t *testing.T) {
	raw, err := schema.Marshal(schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules: []schema.Module{{
			Name: "acl",
			States: map[string]schema.State{
				"present": {Description: "The ACL user exists", Input: schema.Input{
					"hostname": {Type: schema.String, Deprecated: &DeprecatedDef{
						Since: "0.4.0", RemovedIn: "0.6.0", Use: "host",
					}},
					"host": {Type: schema.String, Required: true},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	doc, diags := ParseDocument("schema.json", raw)
	if doc == nil || diag.HasErrors(diags) {
		t.Fatalf("a valid deprecation must survive parsing: %+v", diags)
	}

	// Exactly the walk collectDeprecatedParams performs.
	var def StateDef = mustState(t, doc, "acl", "present")
	p, known := def.Input["hostname"]
	if !known {
		t.Fatal("the deprecated parameter is gone from the state input")
	}
	if p.Deprecated == nil {
		t.Fatal("the deprecation block did not survive the round trip")
	}
	var byValue DeprecatedDef = *p.Deprecated
	if byValue.Since != "0.4.0" || byValue.RemovedIn != "0.6.0" || byValue.Use != "host" {
		t.Fatalf("deprecation fields: %+v", byValue)
	}
	if got := byValue.Notice("hostname"); got !=
		`param "hostname" is deprecated since 0.4.0 and stops working in 0.6.0; use "host" instead` {
		t.Fatalf("Notice text changed: %q", got)
	}
	if DeprecationMinMinors != 2 {
		t.Fatalf("DeprecationMinMinors = %d", DeprecationMinMinors)
	}

	// And the policy still bites: a window shorter than the minimum is refused.
	short, err := schema.Marshal(schema.Document{
		Kind: schema.KindSoulModule, ProtocolVersion: 1,
		Modules: []schema.Module{{Name: "acl", States: map[string]schema.State{
			"present": {Description: "d", Input: schema.Input{
				"hostname": {Type: schema.String, Deprecated: &DeprecatedDef{Since: "0.4.0", RemovedIn: "0.5.0"}},
			}},
		}}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	_, diags = ParseDocument("schema.json", short)
	if !diag.HasErrors(diags) {
		t.Fatal("a deprecation window shorter than the policy must still be refused")
	}
	found := false
	for _, d := range diags {
		if d.Code == "deprecation_window_too_short" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected deprecation_window_too_short, got %+v", diags)
	}
}

func mustState(t *testing.T, doc *Document, module, state string) StateDef {
	t.Helper()
	def, ok := StateOf(doc, module, state)
	if !ok {
		t.Fatalf("%s.%s is missing", module, state)
	}
	return def
}

func TestProtoKind(t *testing.T) {
	want := map[Kind]pluginv1.Kind{
		KindSoulModule:  pluginv1.Kind_KIND_SOUL_MODULE,
		KindSSHProvider: pluginv1.Kind_KIND_SSH_PROVIDER,
		KindSoulBeacon:  pluginv1.Kind_KIND_SOUL_BEACON,
		"wizardry":      pluginv1.Kind_KIND_UNSPECIFIED,
	}
	for k, v := range want {
		if got := ProtoKind(k); got != v {
			t.Fatalf("ProtoKind(%q) = %v, want %v", k, got, v)
		}
	}
}

func TestCapabilityFromString(t *testing.T) {
	want := map[string]pluginv1.Capability{
		"run_as_root":      pluginv1.Capability_CAPABILITY_RUN_AS_ROOT,
		"network_outbound": pluginv1.Capability_CAPABILITY_NETWORK_OUTBOUND,
		"network_inbound":  pluginv1.Capability_CAPABILITY_NETWORK_INBOUND,
		"vault_access":     pluginv1.Capability_CAPABILITY_VAULT_ACCESS,
		"fs_write_root":    pluginv1.Capability_CAPABILITY_FS_WRITE_ROOT,
		"exec_subprocess":  pluginv1.Capability_CAPABILITY_EXEC_SUBPROCESS,
	}
	for in, expected := range want {
		got, ok := CapabilityFromString(in)
		if !ok || got != expected {
			t.Fatalf("CapabilityFromString(%q) = %v,%v", in, got, ok)
		}
	}
	for _, in := range []string{"", "become_root", "RUN_AS_ROOT"} {
		if _, ok := CapabilityFromString(in); ok {
			t.Fatalf("CapabilityFromString(%q) accepted an unknown capability", in)
		}
	}
	// Every capability the SDK lets an author declare must map to the proto enum -
	// otherwise a module could disclose something the host cannot compare.
	for _, c := range schema.AllCapabilities {
		if _, ok := CapabilityFromString(string(c)); !ok {
			t.Fatalf("capability %q has no proto mapping", c)
		}
	}
}
