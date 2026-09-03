package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// parseStateSchema decodes a `state_schema:` block the way the manifest loader
// does, so the fingerprint tests below hash what an author actually wrote rather
// than a Go literal that resembles it.
func parseStateSchema(t *testing.T, src string) InputSchemaMap {
	t.Helper()
	var m InputSchemaMap
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("parse state_schema: %v\n%s", err, src)
	}
	return m
}

// TestFingerprint_IsOverTheSchemaAndNotTheText is the property the whole artifact
// rests on: the hash moves when the STRUCTURE moves and stays put otherwise.
//
// Each case below is a real edit to a real block, not a hand-built Go value — a
// canonicalizer that quietly hashed the source bytes would pass a test written the
// other way round.
func TestFingerprint_IsOverTheSchemaAndNotTheText(t *testing.T) {
	const base = `
users:
  type: array
  required: true
  items:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string }
maxmemory_mb:
  type: integer
  min: 64
`
	want := FingerprintStateSchema(parseStateSchema(t, base))

	same := map[string]string{
		"a comment above a field": `
users:
  # who may connect - added while reviewing, changes nothing
  type: array
  required: true
  items:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string }
maxmemory_mb:
  type: integer
  min: 64
`,
		"the two top-level fields swapped": `
maxmemory_mb:
  type: integer
  min: 64
users:
  type: array
  required: true
  items:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string }
`,
		"keys reordered inside one node": `
users:
  required: true
  items:
    type: object
    properties:
      perms: { type: string }
      name: { required: true, type: string }
  type: array
maxmemory_mb:
  min: 64
  type: integer
`,
		"block form instead of flow form": `
users:
  type: array
  required: true
  items:
    type: object
    properties:
      name:
        type: string
        required: true
      perms:
        type: string
maxmemory_mb:
  type: integer
  min: 64
`,
	}
	for name, src := range same {
		t.Run("unchanged: "+name, func(t *testing.T) {
			if got := FingerprintStateSchema(parseStateSchema(t, src)); got != want {
				t.Errorf("fingerprint moved for a change that is not structural:\n got  %s\n want %s\n canonical:\n%s\n%s",
					got, want, canonicalStateSchema(parseStateSchema(t, src)), canonicalStateSchema(parseStateSchema(t, base)))
			}
		})
	}

	differs := map[string]string{
		"a field added": `
users:
  type: array
  required: true
  items:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string }
maxmemory_mb:
  type: integer
  min: 64
seeded_from:
  type: string
`,
		"a field removed": `
users:
  type: array
  required: true
  items:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string }
`,
		"a field renamed": `
users:
  type: array
  required: true
  items:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string }
maxmemory_bytes:
  type: integer
  min: 64
`,
		"a nested property added": `
users:
  type: array
  required: true
  items:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string }
      state: { type: string }
maxmemory_mb:
  type: integer
  min: 64
`,
		"a type changed": `
users:
  type: array
  required: true
  items:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string }
maxmemory_mb:
  type: string
  min: 64
`,
		"a constraint changed": `
users:
  type: array
  required: true
  items:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string }
maxmemory_mb:
  type: integer
  min: 128
`,
		"required dropped": `
users:
  type: array
  items:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string }
maxmemory_mb:
  type: integer
  min: 64
`,
	}
	for name, src := range differs {
		t.Run("changed: "+name, func(t *testing.T) {
			if got := FingerprintStateSchema(parseStateSchema(t, src)); got == want {
				t.Errorf("fingerprint did not move for a structural change (%s)", got)
			}
		})
	}
}

// TestFingerprint_SeparatorsInAuthoredNamesDoNotCollide — the canonical form uses
// `=` and `;` as its own separators, so every string that comes from the DOCUMENT
// (a field name, a description, a key inside a `default:`) is length-prefixed. Drop
// the prefix and the two schemas below render to identical bytes.
//
// Nothing else in this file writes a separator into an authored name, so without
// this case the length prefix could be removed and the only failure would be the
// corpus golden — whose natural repair is a re-stamp, after which the collision is
// live and green.
func TestFingerprint_SeparatorsInAuthoredNamesDoNotCollide(t *testing.T) {
	// Each pair is built to render to the SAME bytes if the prefix goes: the second
	// member smuggles the first member's own separators inside one authored string.
	// That construction is coupled to the current rendering on purpose — an
	// injectivity claim can only be tested against the encoding that makes it.
	pairs := [][2]string{
		{ // two fields, against one field whose NAME is both of them
			"a: { type: string }\nb: { type: string }\n",
			"\"a={type=string;};b\": { type: string }\n",
		},
		{ // two keys of one node, against one key whose VALUE is both of them
			"x: { type: string, description: y, pattern: z }\n",
			"x: { type: string, description: \"y;pattern=z\" }\n",
		},
		{ // the same, one level down, where the names come from a nested object
			"o: { type: object, properties: { a: { type: string }, b: { type: string } } }\n",
			"o: { type: object, properties: { \"a={type=string;};b\": { type: string } } }\n",
		},
	}
	for i, pair := range pairs {
		first := FingerprintStateSchema(parseStateSchema(t, pair[0]))
		second := FingerprintStateSchema(parseStateSchema(t, pair[1]))
		if first == second {
			t.Errorf("pair %d hashes alike (%s):\n%s\n---\n%s\ncanonical:\n%s\n%s", i, first, pair[0], pair[1],
				canonicalStateSchema(parseStateSchema(t, pair[0])), canonicalStateSchema(parseStateSchema(t, pair[1])))
		}
	}
}

// TestFingerprint_AStructWithNoExportedFieldsIsNotCollapsed — goccy decodes an
// explicit `!!timestamp` to a `time.Time`, whose fields are all unexported. Rendered
// by the struct walk alone that is `{}` for every date, and `{}` again for an empty
// map: three different `default:` values, one hash.
func TestFingerprint_AStructWithNoExportedFieldsIsNotCollapsed(t *testing.T) {
	seen := map[string]string{}
	for _, src := range []string{
		"created: { type: string, default: !!timestamp 2026-01-01 }\n",
		"created: { type: string, default: !!timestamp 2099-12-31 }\n",
		"created: { type: string, default: {} }\n",
	} {
		got := FingerprintStateSchema(parseStateSchema(t, src))
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q hash alike (%s)", prev, src, got)
		}
		seen[got] = src
	}
}

// TestFingerprint_IsStableAcrossRuns — Go randomises map iteration order per range,
// so an unsorted walk produces a different hash roughly every time it is called. A
// lock stamped by one run and checked by the next would then be stale at random.
func TestFingerprint_IsStableAcrossRuns(t *testing.T) {
	var src strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&src, "field_%02d:\n  type: object\n  properties:\n    a: { type: string }\n    b: { type: integer }\n", i)
	}
	schema := parseStateSchema(t, src.String())

	first := FingerprintStateSchema(schema)
	for i := 0; i < 50; i++ {
		if got := FingerprintStateSchema(schema); got != first {
			t.Fatalf("fingerprint %d = %s, first = %s (map iteration order is leaking into the hash)", i, got, first)
		}
	}
	// And a second decode of the same text, so the stability is a property of the
	// schema and not of one decoded value's internal layout.
	if got := FingerprintStateSchema(parseStateSchema(t, src.String())); got != first {
		t.Errorf("a second parse of the same text hashed to %s, want %s", got, first)
	}
}

// TestFingerprint_EveryInputSchemaFieldParticipates is the guard on the ONE way this
// file can rot: a field added to the DSL that the canonical walk does not see. The
// lock would go on matching across an edit to it, silently, and nothing else in the
// repository would notice — the fingerprint is the last thing anybody would suspect.
//
// The walk is by reflection precisely so it cannot happen; this test is what keeps
// that true if someone ever replaces it with a hand-written field list.
func TestFingerprint_EveryInputSchemaFieldParticipates(t *testing.T) {
	base := &InputSchema{Type: "string"}
	baseFingerprint := FingerprintStateSchema(InputSchemaMap{"probe": base})

	rt := reflect.TypeOf(InputSchema{})
	var exported int
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			// rawRequired is unexported ON PURPOSE: it is the original `required:`
			// AST node and carries a line and a column, so hashing it would make a
			// comment inserted above the key move the fingerprint.
			continue
		}
		exported++
		t.Run(f.Name, func(t *testing.T) {
			mutated := *base
			setNonZeroForFingerprint(t, reflect.ValueOf(&mutated).Elem().Field(i))
			if got := FingerprintStateSchema(InputSchemaMap{"probe": &mutated}); got == baseFingerprint {
				t.Errorf("setting %s.%s does not move the fingerprint - the canonical walk does not see this field\ncanonical: %s",
					rt.Name(), f.Name, canonicalStateSchema(InputSchemaMap{"probe": &mutated}))
			}
		})
	}
	if exported == 0 {
		t.Fatal("no exported fields found on InputSchema - the reflection above is not doing anything")
	}
}

// TestFingerprint_NestedSchemaContentParticipates — the field-participation test
// above only ALLOCATES a pointed-to struct, so it would pass against a walk that
// rendered every `items:` and every `source:` as `{}`. These cases say the content
// under those keys is hashed, not merely their presence.
func TestFingerprint_NestedSchemaContentParticipates(t *testing.T) {
	nested := map[string][2]string{
		"items": {
			"users: { type: array, items: { type: object, properties: { name: { type: string } } } }\n",
			"users: { type: array, items: { type: object, properties: { name: { type: integer } } } }\n",
		},
		"properties": {
			"tls: { type: object, properties: { enable: { type: boolean } } }\n",
			"tls: { type: object, properties: { enable: { type: boolean, required: true } } }\n",
		},
		"additional_properties as a schema": {
			"sysctl: { type: object, additional_properties: { type: string } }\n",
			"sysctl: { type: object, additional_properties: { type: integer } }\n",
		},
		"additional_properties bool against a schema": {
			"sysctl: { type: object, additional_properties: true }\n",
			"sysctl: { type: object, additional_properties: { type: string } }\n",
		},
	}
	for name, pair := range nested {
		t.Run(name, func(t *testing.T) {
			if FingerprintStateSchema(parseStateSchema(t, pair[0])) == FingerprintStateSchema(parseStateSchema(t, pair[1])) {
				t.Errorf("the walk does not descend here:\n%s---\n%s", pair[0], pair[1])
			}
		})
	}

	// `Source` is a struct behind a pointer, so the same blind spot is reachable a
	// second way: every one of its fields must move the hash.
	rt := reflect.TypeOf(InputSource{})
	base := &InputSchema{Type: "string", Source: &InputSource{}}
	baseFingerprint := FingerprintStateSchema(InputSchemaMap{"probe": base})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		t.Run("Source."+f.Name, func(t *testing.T) {
			source := InputSource{}
			setNonZeroForFingerprint(t, reflect.ValueOf(&source).Elem().Field(i))
			mutated := *base
			mutated.Source = &source
			if FingerprintStateSchema(InputSchemaMap{"probe": &mutated}) == baseFingerprint {
				t.Errorf("setting InputSource.%s does not move the fingerprint", f.Name)
			}
		})
	}
}

// TestFingerprint_CanonicalFieldNamesAreUnique — two fields rendering under one name
// would make their edits indistinguishable in the hash, which is the same blind spot
// as not hashing one of them at all.
func TestFingerprint_CanonicalFieldNamesAreUnique(t *testing.T) {
	for _, rt := range []reflect.Type{reflect.TypeOf(InputSchema{}), reflect.TypeOf(InputSource{})} {
		seen := map[string]string{}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			name := canonicalFieldName(f)
			if prev, dup := seen[name]; dup {
				t.Errorf("%s.%s and %s.%s both canonicalize to %q", rt.Name(), prev, rt.Name(), f.Name, name)
			}
			seen[name] = f.Name
		}
	}
}

// setNonZeroForFingerprint gives a field a value distinguishable from its zero one.
// A pointer to a struct is only ALLOCATED — descending into `Items *InputSchema`
// would recurse forever, and a non-nil pointer is already the difference the test is
// looking for.
func setNonZeroForFingerprint(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("canary")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.Interface:
		v.Set(reflect.ValueOf("canary"))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		if v.Elem().Kind() != reflect.Struct {
			setNonZeroForFingerprint(t, v.Elem())
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		setNonZeroForFingerprint(t, v.Index(0))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		setNonZeroForFingerprint(t, key)
		val := reflect.New(v.Type().Elem()).Elem()
		setNonZeroForFingerprint(t, val)
		m.SetMapIndex(key, val)
		v.Set(m)
	default:
		t.Fatalf("no non-zero value for kind %s - teach this helper about it, do not skip the field", v.Kind())
	}
}

// TestSchemaLock_RoundTrip — what the stamp writes is what the check reads.
func TestSchemaLock_RoundTrip(t *testing.T) {
	want := SchemaLock{Version: 15, Fingerprint: FingerprintStateSchema(parseStateSchema(t, "x: { type: string }\n"))}
	data := MarshalSchemaLock(want)
	got, diags := ParseSchemaLock("schema.lock", data)
	if diag.HasErrors(diags) {
		t.Fatalf("the generated lock does not parse: %v", diags)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if second := MarshalSchemaLock(want); string(second) != string(data) {
		t.Errorf("MarshalSchemaLock is not deterministic:\n%s\n%s", data, second)
	}
}

// TestParseSchemaLock_RefusesWhatItCannotTrust — the lock is generated, so anything
// it cannot read is answered by a re-stamp rather than by using what is left. Reading
// half of it would mean checking half the invariant, silently.
func TestParseSchemaLock_RefusesWhatItCannotTrust(t *testing.T) {
	for name, body := range map[string]string{
		"not YAML at all":   "version: [",
		"an unknown key":    "version: 2\nfingerprint: sha256:ab\nnote: hand-edited\n",
		"no version":        "fingerprint: sha256:ab\n",
		"version below one": "version: 0\nfingerprint: sha256:ab\n",
		"no fingerprint":    "version: 2\n",
		"an unknown digest": "version: 2\nfingerprint: md5:abcd\n",
		"a bare hex digest": "version: 2\nfingerprint: abcd\n",
		"a non-hex digest":  "version: 2\nfingerprint: sha256:zzzz\n",
		// Valid hex, wrong length. Without the length check this parses, and the
		// mangled file is then reported as `schema_lock_stale` — sending the author
		// to write a migration step for an edit that never happened.
		"a truncated digest":  "version: 2\nfingerprint: sha256:abcd\n",
		"an empty hex digest": "version: 2\nfingerprint: 'sha256:'\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, diags := ParseSchemaLock("schema.lock", []byte(body))
			if !diag.HasErrors(diags) {
				t.Fatalf("accepted %q", body)
			}
			if diags[0].Code != "schema_lock_malformed" {
				t.Errorf("code = %q, want schema_lock_malformed", diags[0].Code)
			}
		})
	}
}

// TestValidateSchemaLock_SilentWithoutALadderOrALock pins the boundary of the
// requirement, which is the one decision in this file a reader is most likely to
// change by accident.
//
// A directory that merely holds a `service.yml` — a fixture, a snippet under review,
// a service still at version 1 that has never stamped — is NOT a service repository
// that adopted the lock, and demanding one from it would fail documents that own no
// repository at all. Adoption is one `schema-stamp`; after it, the lock is present
// and checked from then on, and the case below stops applying.
func TestValidateSchemaLock_SilentWithoutALadderOrALock(t *testing.T) {
	schema := parseStateSchema(t, "greeting: { type: string }\n")

	t.Run("no migrations directory", func(t *testing.T) {
		if got := ValidateSchemaLock(t.TempDir(), schema); len(got) != 0 {
			t.Errorf("diagnostics on a tree that never adopted the lock: %v", got)
		}
	})
	t.Run("an empty ladder", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, MigrationsDirName), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ValidateSchemaLock(root, schema); len(got) != 0 {
			t.Errorf("diagnostics on an empty ladder: %v", got)
		}
	})
	t.Run("a stamped tree with an empty ladder is still checked", func(t *testing.T) {
		root := t.TempDir()
		if _, err := StampSchemaLock(root, schema, BaseStateSchemaVersion); err != nil {
			t.Fatal(err)
		}
		if got := ValidateSchemaLock(root, schema); len(got) != 0 {
			t.Fatalf("a freshly stamped tree is not clean: %v", got)
		}
		// The lock is present now, so it is compared whether or not a rung exists —
		// which is what makes adoption a one-way door.
		edited := parseStateSchema(t, "greeting: { type: string }\nfarewell: { type: string }\n")
		got := ValidateSchemaLock(root, edited)
		if len(got) != 1 || got[0].Code != "schema_lock_stale" {
			t.Fatalf("an edited schema over a stamped v1 tree = %v, want one schema_lock_stale", got)
		}
	})
}

// TestValidateSchemaLock_VersionIsNotJudgedOverABrokenLadder — red for the wrong
// reason is worse than silence here. A ladder with a gap derives its version from
// whatever survived, so comparing the stamp against it would send the author to
// re-stamp a ladder they have to repair first — and the repair moves the number
// again.
func TestValidateSchemaLock_VersionIsNotJudgedOverABrokenLadder(t *testing.T) {
	root := t.TempDir()
	schema := parseStateSchema(t, "greeting: { type: string }\n")
	writeStep := func(dir string) {
		t.Helper()
		full := filepath.Join(root, MigrationsDirName, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, MigrationStepFile), []byte("transform: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeStep("002_first")
	writeStep("003_second")
	if _, err := StampSchemaLock(root, schema, 3); err != nil {
		t.Fatal(err)
	}
	if got := ValidateSchemaLock(root, schema); len(got) != 0 {
		t.Fatalf("a stamped, gapless ladder is not clean: %v", got)
	}

	// The mistake an author actually makes: the next step is numbered 005 instead of
	// 004. That is a gap AND a moved top at once — the derived version jumps to 5
	// while the stamp still says 3 — so a lock that judged it would report the
	// version as stale and send the author to re-stamp, when the fix is to renumber
	// the step and the number moves again anyway.
	writeStep("005_misnumbered")
	if got := ValidateSchemaLock(root, schema); len(got) != 0 {
		t.Errorf("the lock judged a broken ladder: %v", got)
	}
	if ladder, _ := ScanMigrationLadder(root); ladder.Version() == 3 {
		t.Fatal("the derived version did not move - this case is not testing the suppression at all")
	}
	if !diag.HasErrors(ValidateMigrationLadder(root)) {
		t.Error("the gap itself went unreported - the suppression above would then hide it entirely")
	}

	// The FINGERPRINT half is not suppressed with it. It reads no ladder, so a schema
	// edited while the ladder happens to be broken must still be reported — otherwise
	// one gap silently switches off the check the lock exists for, for as long as the
	// gap lasts.
	edited := parseStateSchema(t, "greeting: { type: string }\nfarewell: { type: string }\n")
	got := ValidateSchemaLock(root, edited)
	if len(got) != 1 || got[0].Code != "schema_lock_stale" {
		t.Fatalf("an edited schema over a broken ladder = %v, want exactly one schema_lock_stale", got)
	}
}

// TestValidateSchemaLock_MissingAndUnreadable — the two codes that say the
// comparison could not happen at all.
//
// `schema_lock_missing` is the whole of "adoption is a one-way door": a service that
// has stamped once cannot quietly stop being checked, because deleting the file is
// itself an error. Without a case here the entire branch could be deleted — making
// an absent lock silent forever — with every other test in the repository still
// green.
func TestValidateSchemaLock_MissingAndUnreadable(t *testing.T) {
	schema := parseStateSchema(t, "greeting: { type: string }\n")
	withLadder := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		step := filepath.Join(root, MigrationsDirName, "002_first")
		if err := os.MkdirAll(step, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(step, MigrationStepFile), []byte("transform: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}

	t.Run("a ladder with no lock beside it", func(t *testing.T) {
		root := withLadder(t)
		got := ValidateSchemaLock(root, schema)
		if len(got) != 1 || got[0].Code != "schema_lock_missing" {
			t.Fatalf("= %v, want one schema_lock_missing", got)
		}
		if got[0].Level != diag.LevelError {
			t.Errorf("level = %s, want error: a check that is not running must not read as green", got[0].Level)
		}
		if !strings.Contains(got[0].Hint, "schema-stamp") {
			t.Errorf("hint %q does not name the command that fixes it", got[0].Hint)
		}
	})

	t.Run("a lock that cannot be read", func(t *testing.T) {
		root := withLadder(t)
		// A DIRECTORY where the file goes: an EISDIR that does not depend on the uid
		// the tests run as, unlike a chmod.
		if err := os.Mkdir(SchemaLockPath(root), 0o755); err != nil {
			t.Fatal(err)
		}
		got := ValidateSchemaLock(root, schema)
		if len(got) != 1 || got[0].Code != "schema_lock_unreadable" {
			t.Fatalf("= %v, want one schema_lock_unreadable", got)
		}
		if got[0].Level != diag.LevelError {
			t.Errorf("level = %s, want error", got[0].Level)
		}
	})
}

// TestValidateSchemaLock_VersionIsJudgedEvenWhenTheSchemaCannotBeHashed — the two
// halves read different things, so an unresolved `$type` may not take the version
// comparison down with it.
//
// The version half is two integers and no schema. A published step deleted off the
// top of the ladder is the one failure nothing else in the repository can see, and
// it must not stop being reported because a type reference elsewhere in the manifest
// went missing.
func TestValidateSchemaLock_VersionIsJudgedEvenWhenTheSchemaCannotBeHashed(t *testing.T) {
	root := t.TempDir()
	step := filepath.Join(root, MigrationsDirName, "002_first")
	if err := os.MkdirAll(step, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(step, MigrationStepFile), []byte("transform: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unresolved := parseStateSchema(t, "accounts: { $type: AclUser }\n")
	if !SchemaHasTypeRef(unresolved) {
		t.Fatal("the fixture resolved - this case is not testing the unresolved path")
	}
	// Stamped at 3 while the ladder tops out at 2: a step was deleted.
	if _, err := StampSchemaLock(root, parseStateSchema(t, "x: { type: string }\n"), 3); err != nil {
		t.Fatal(err)
	}

	got := ValidateSchemaLock(root, unresolved)
	codes := map[string]diag.Level{}
	for _, d := range got {
		codes[d.Code] = d.Level
	}
	if _, ok := codes["schema_lock_version_stale"]; !ok {
		t.Errorf("the deleted step went unreported because a $type was unresolved: %v", got)
	}
	if lvl, ok := codes["schema_lock_unchecked"]; !ok {
		t.Errorf("the fingerprint half did not say it could not run: %v", got)
	} else if lvl != diag.LevelError {
		// A warning here is a green run over a schema nobody compared — and one
		// route to it (a types.yml that exists and cannot be read) is itself only a
		// warning upstream.
		t.Errorf("schema_lock_unchecked level = %s, want error", lvl)
	}
	if _, ok := codes["schema_lock_stale"]; ok {
		t.Errorf("half a schema was hashed and reported as an edit: %v", got)
	}
}
