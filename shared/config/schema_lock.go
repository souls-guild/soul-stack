package config

// The schema lock: a generated stamp of a service's `state_schema` beside its
// migration ladder ([ADR-019] amendment 2026-09-01, clause 4).
//
// It exists because a service describes the shape of `incarnation.state` TWICE —
// declaratively in `state_schema`, imperatively in the `migrations/` ladder — both
// by hand, with nothing reconciling them. Nothing reconciles them offline, and the
// upgrade transaction does not either: it applies the chain and never loads the
// target schema. So a schema edit that no step implements is invisible until an
// incarnation is upgraded and its state comes out in a shape the schema does not
// describe. The stamped `fingerprint` catches that edit; the stamped `version`
// catches the other direction — a step deleted off the top of a published ladder,
// which under a derived version moves the service's version with nothing left in the
// repository to contradict it.
//
// ★ The fingerprint proves a step was ADDED, not that the step is correct. What a
// step actually does to state is its own `tests/`.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// SchemaLockFile is the generated stamp, inside `migrations/` beside the steps.
// [ScanMigrationLadder] passes it over in silence: it is not named like a step and
// never was.
const SchemaLockFile = "schema.lock"

// schemaFingerprintAlgo prefixes every fingerprint, so the document says which hash
// it holds instead of leaving a reader to count hex digits. A lock carrying an
// algorithm this build does not know is malformed rather than mismatched — the
// answer there is a re-stamp, not a migration step.
const schemaFingerprintAlgo = "sha256"

// SchemaLock is `migrations/schema.lock`, read.
//
// Version is the top of the ladder at stamp time and Fingerprint is
// [FingerprintStateSchema] of the manifest's `state_schema` at the same moment.
// Both are compared on every `soul-lint validate-service`; neither is read by the
// keeper, which derives the version from the ladder itself.
type SchemaLock struct {
	Version     int    `yaml:"version"`
	Fingerprint string `yaml:"fingerprint"`
}

// SchemaLockPath is where the lock lives for a service rooted at serviceRoot.
func SchemaLockPath(serviceRoot string) string {
	return filepath.Join(serviceRoot, MigrationsDirName, SchemaLockFile)
}

// FingerprintStateSchema hashes a PARSED `state_schema`, never the bytes it was
// written in.
//
// That is the whole point of the artifact and not an optimisation: a text hash would
// move when a comment is added, when two fields are swapped, when a value is
// re-quoted — and an author who has to re-stamp for a comment stops reading what a
// re-stamp means. What is hashed is the decoded [InputSchemaMap] rendered in a
// canonical form: keys sorted, absent keys absent, nothing carrying a position in
// the file.
//
// The schema must be `$type`-RESOLVED first. An unresolved `{$type: AclUser}` node
// declares no shape, so hashing one would stamp half a schema and go on matching
// while the named type changed underneath it. Callers that cannot resolve report the
// check as not run — see [ValidateSchemaLock].
func FingerprintStateSchema(schema InputSchemaMap) string {
	sum := sha256.Sum256(canonicalStateSchema(schema))
	return schemaFingerprintAlgo + ":" + hex.EncodeToString(sum[:])
}

// canonicalStateSchema renders a schema in the form that gets hashed.
//
// Split out from [FingerprintStateSchema] so a test can read what is being hashed:
// a fingerprint that changed for the wrong reason is undiagnosable from the hex
// alone, and "the fingerprint moved" is the one symptom this file has to be able to
// explain.
func canonicalStateSchema(schema InputSchemaMap) []byte {
	var b bytes.Buffer
	writeCanonicalValue(&b, reflect.ValueOf(schema))
	return b.Bytes()
}

// writeCanonicalValue renders one value of the parsed schema.
//
// It walks by REFLECTION rather than by a hand-written field list, and that is the
// property worth keeping. A hand-written list is a second description of
// [InputSchema] — the very shape of defect this whole artifact exists to catch — and
// it rots silently: a field added to the DSL in a year would simply not be hashed,
// and the lock would go on matching across a schema edit it was blind to. Reflection
// cannot develop that blind spot; [TestFingerprint_EveryInputSchemaFieldParticipates]
// holds the claim.
//
// Three rules make it stable:
//
//   - a key is the field's YAML spelling, so renaming a Go field does not re-stamp
//     every service repository on the planet;
//   - a zero field is OMITTED, so ADDING a field to the DSL leaves every existing
//     fingerprint where it was — only schemas that actually write the new key move.
//     The one distinction that costs is `default: null`, which decodes to a nil
//     `any` and is therefore indistinguishable from no `default:` at all; telling
//     them apart would need the AST, which is the position-carrying thing this
//     function exists to stay away from;
//   - unexported fields are skipped, which is what keeps `rawRequired` — the
//     original `required:` AST node, carrying its line and column — out of the hash.
//     Hashing it would make a fingerprint move when a comment above it did, which is
//     exactly what this function refuses to do.
//
// A value reached through an `any` field carries its STRUCTURE and not its YAML
// tag, which is deliberate for `2` against `2.0` and is the accepted cost for the
// exotic rest of it: `default: !!binary "aGk="` decodes to a []byte and renders like
// the list `[104, 105]`. That is a missed re-stamp between two spellings of one
// `default:`, never a missed field.
//
// No depth guard: a resolved state_schema is a tree (a `$type` cycle is refused
// before resolution, and an unresolved schema is never hashed), and the linter's
// stated limit on a schema recursive enough to exhaust the stack is unchanged
// (soul-lint/internal/validate/tree.go → safeDiags).
func writeCanonicalValue(b *bytes.Buffer, v reflect.Value) {
	switch v.Kind() {
	case reflect.Invalid:
		b.WriteString("~")
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			b.WriteString("~")
			return
		}
		writeCanonicalValue(b, v.Elem())
	case reflect.Map:
		writeCanonicalMap(b, v)
	case reflect.Slice, reflect.Array:
		b.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			writeCanonicalValue(b, v.Index(i))
			b.WriteByte(';')
		}
		b.WriteByte(']')
	case reflect.Struct:
		writeCanonicalStruct(b, v)
	case reflect.String:
		writeCanonicalString(b, v.String())
	case reflect.Bool:
		b.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		// 'g' with -1 precision is the shortest form that round-trips, so 2 and 2.0
		// hash alike — which they must: the YAML decoder decides between them by
		// spelling, and a re-quoted number is not a schema change.
		b.WriteString(strconv.FormatFloat(v.Float(), 'g', -1, 64))
	default:
		// A channel, a function, a complex number: unreachable in a decoded YAML
		// document. Rendered as one opaque token rather than panicking — a
		// fingerprint is not worth taking a lint run down for — and distinct from
		// every other form above, so it cannot be mistaken for an absent value.
		b.WriteString("?" + v.Kind().String())
	}
}

// writeCanonicalMap renders a map with its keys SORTED. Go's map iteration order is
// randomised per run, so this is the difference between a fingerprint and a coin
// toss. Keys are compared by their own canonical rendering, which keeps a non-string
// key (impossible in a schema, reachable in a decoded `default:`) deterministic too.
func writeCanonicalMap(b *bytes.Buffer, v reflect.Value) {
	type entry struct {
		key string
		val reflect.Value
	}
	entries := make([]entry, 0, v.Len())
	iter := v.MapRange()
	for iter.Next() {
		var kb bytes.Buffer
		writeCanonicalValue(&kb, iter.Key())
		entries = append(entries, entry{key: kb.String(), val: iter.Value()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	b.WriteByte('{')
	for _, e := range entries {
		b.WriteString(e.key)
		b.WriteByte('=')
		writeCanonicalValue(b, e.val)
		b.WriteByte(';')
	}
	b.WriteByte('}')
}

// writeCanonicalStruct renders [InputSchema] and [InputSource]: every exported field
// that is not at its zero value, keyed by its YAML spelling and sorted by it.
//
// A struct with NO exported fields is rendered by its type and its printed value
// instead. That is not hypothetical: goccy decodes an explicit `!!timestamp` tag to
// a `time.Time`, whose three fields are all unexported, so a `default:` holding one
// date would otherwise render `{}` — identical to another date, and identical to an
// empty map. Rendering the type as well as the value keeps the two apart from each
// other and from `{}`.
func writeCanonicalStruct(b *bytes.Buffer, v reflect.Value) {
	t := v.Type()
	if !hasExportedField(t) {
		b.WriteString("!" + t.String() + ":")
		if v.CanInterface() {
			writeCanonicalString(b, fmt.Sprintf("%v", v.Interface()))
			return
		}
		// Unreachable from a decoded document — such a value is only reached through
		// an `any` field, which is exported. Written rather than dropped so that an
		// opaque value is never silently equal to an empty one.
		b.WriteString("?")
		return
	}
	type field struct {
		name string
		val  reflect.Value
	}
	fields := make([]field, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		if fv.IsZero() {
			continue
		}
		fields = append(fields, field{name: canonicalFieldName(f), val: fv})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })

	b.WriteByte('{')
	for _, f := range fields {
		b.WriteString(f.name)
		b.WriteByte('=')
		writeCanonicalValue(b, f.val)
		b.WriteByte(';')
	}
	b.WriteByte('}')
}

// canonicalFieldName is the field's YAML key, or a snake_case of its Go name when
// the tag says `-`.
//
// The two fields tagged `-` are authored keys all the same: `Required` is filled by
// [InputSchema.UnmarshalYAML] so the reflect-walker does not trip over `required:`,
// and `TypeRef` because `$type` is not a legal Go tag. Both belong in the hash, and
// naming them by their Go spelling is fine — the name is a label inside a hash
// input, and it only has to be stable and distinct.
func canonicalFieldName(f reflect.StructField) string {
	tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if tag != "" && tag != "-" {
		return tag
	}
	return snakeCase(f.Name)
}

// hasExportedField reports whether any field of the struct type would be rendered.
func hasExportedField(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			return true
		}
	}
	return false
}

func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// writeCanonicalString length-prefixes the value, so that no two different schemas
// can render to the same bytes by splitting a string across the separators. Without
// it a field named `a=b;c` would be indistinguishable from two fields.
func writeCanonicalString(b *bytes.Buffer, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
}

// MarshalSchemaLock renders the lock as the file that gets committed.
//
// Byte-identical for identical input, with no timestamp and no path in it: the whole
// value of a generated file in git is that a re-run produces no diff unless
// something it describes moved.
func MarshalSchemaLock(lock SchemaLock) []byte {
	var b bytes.Buffer
	b.WriteString("# GENERATED by `soul-lint schema-stamp` - do not edit by hand.\n")
	b.WriteString("#\n")
	b.WriteString("# version:     the top of the migrations/ ladder when this was stamped.\n")
	b.WriteString("# fingerprint: a hash of the PARSED and canonicalized state_schema of\n")
	b.WriteString("#              service.yml - never of the file text, so a comment or a\n")
	b.WriteString("#              reordered key does not move it.\n")
	fmt.Fprintf(&b, "version: %d\n", lock.Version)
	fmt.Fprintf(&b, "fingerprint: %s\n", lock.Fingerprint)
	return b.Bytes()
}

// ParseSchemaLock reads the lock document. path addresses the diagnostics.
//
// Decoding is strict: this file is generated, so an unknown key in it is a
// hand-edit or a lock from a build that knew something this one does not, and both
// are answered by a re-stamp rather than by reading what is left.
func ParseSchemaLock(path string, data []byte) (SchemaLock, []diag.Diagnostic) {
	malformed := func(why, hint string) []diag.Diagnostic {
		return []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File: path, Code: "schema_lock_malformed",
			Message: fmt.Sprintf("%s cannot be read as a schema lock: %s", SchemaLockFile, why),
			Hint:    hint,
		}}
	}
	const restamp = "the file is generated - run `soul-lint schema-stamp <service dir>` and commit the result"

	var lock SchemaLock
	if err := yaml.UnmarshalWithOptions(stripBOM(data), &lock, yaml.Strict()); err != nil {
		return SchemaLock{}, malformed(err.Error(), restamp)
	}
	if lock.Version < BaseStateSchemaVersion {
		return SchemaLock{}, malformed(
			fmt.Sprintf("version: %d, and no service is below %d", lock.Version, BaseStateSchemaVersion),
			fmt.Sprintf("version: is the top of the ladder at stamp time; an empty migrations/ is version %d - %s", BaseStateSchemaVersion, restamp))
	}
	algo, hexDigest, ok := strings.Cut(lock.Fingerprint, ":")
	// The LENGTH is checked, not merely the alphabet. A truncated digest is a
	// mangled file, and reporting it as `schema_lock_stale` would send the author
	// to write a migration step for a schema edit that never happened.
	if !ok || algo != schemaFingerprintAlgo || len(hexDigest) != sha256.Size*2 {
		return SchemaLock{}, malformed(
			fmt.Sprintf("fingerprint: %q is not a %s:<%d hex digits> digest this build writes", lock.Fingerprint, schemaFingerprintAlgo, sha256.Size*2),
			restamp)
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return SchemaLock{}, malformed(fmt.Sprintf("fingerprint: %q is not hexadecimal", lock.Fingerprint), restamp)
	}
	return lock, nil
}

// ValidateSchemaLock compares `migrations/schema.lock` against the service as it
// now stands: its `state_schema` (already `$type`-resolved by the caller) and the
// top of its ladder.
//
// **When a lock is expected.** A lock that is THERE is always checked. A lock that
// is absent is an error once the ladder holds at least one step, and says nothing
// at all otherwise. That boundary is deliberate and it is where the artifact's own
// bypass argument lands ([ADR-019] clause 4): a service adopts the lock with one
// `schema-stamp`, and from then on it cannot quietly stop being checked — deleting
// the file is `schema_lock_missing`, and the only way back to silence is deleting
// the ladder itself, which is not a change anyone makes by accident. Demanding a
// lock from every directory that merely holds a `service.yml` would instead make a
// loose manifest — a fixture, a snippet under review — fail for owning no
// repository.
//
// **The two halves are judged independently**, because they read different things.
// The version comparison is suppressed when the ladder's LAYOUT OR CONTINUITY is
// broken — the derived version is then the top of whatever survived, and reporting
// the lock as stale on top of a `migration_chain_broken` would send the author to
// re-stamp a ladder they have to repair first, red for the wrong reason. (A step
// whose `main.yml` is missing does NOT suppress it: the rung is on disk, so the top
// is where it says it is.) The fingerprint comparison is independent of the ladder
// and never suppressed by it; the schema being unresolvable suppresses that half
// alone, and the version comparison still runs underneath it.
//
// The ladder's own diagnostics are NOT returned here: `soul-lint` runs
// [ValidateMigrationLadder] beside this call and returning them twice would double
// every finding.
func ValidateSchemaLock(serviceRoot string, schema InputSchemaMap) []diag.Diagnostic {
	lockPath := SchemaLockPath(serviceRoot)
	ladder, ladderDiags := ScanMigrationLadder(serviceRoot)
	// Continuity too, and not the scan alone: a gap is what the scan does not see,
	// and a gap is exactly the shape of breakage that moves the top of the ladder
	// while the author's real fix is to renumber rather than to re-stamp.
	ladderDiags = append(ladderDiags, ladderContinuityDiags(serviceRoot, ladder)...)

	data, err := os.ReadFile(lockPath)
	switch {
	case err == nil:
	case !errors.Is(err, fs.ErrNotExist):
		return []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File: lockPath, Code: "schema_lock_unreadable",
			// Not "is present but cannot be read": the commonest way here is
			// `migrations` committed as a regular file, where the lock is not present
			// at all and the wrapped error is what says so.
			Message: fmt.Sprintf("%s/%s cannot be read: %v", MigrationsDirName, SchemaLockFile, err),
			Hint:    "the stamp is what tells a state_schema edit apart from one with a migration behind it; a lock that cannot be read checks nothing",
		}}
	case len(ladder.Steps) == 0:
		// No ladder and no lock: nothing has been adopted here, and there is no
		// second description of the state to reconcile against.
		return nil
	default:
		return []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			File: lockPath, Code: "schema_lock_missing",
			Message: fmt.Sprintf("the ladder has %d step(s) and there is no %s/%s", len(ladder.Steps), MigrationsDirName, SchemaLockFile),
			Hint:    "run `soul-lint schema-stamp <service dir>` and commit the result: without the stamp, a state_schema edited with no ladder step behind it is caught by nothing, offline or online",
		}}
	}

	lock, lockDiags := ParseSchemaLock(lockPath, data)
	if diag.HasErrors(lockDiags) {
		// Nothing to compare against. Reporting a mismatch beside an unreadable
		// stamp would name two mistakes where there is one.
		return lockDiags
	}

	var out []diag.Diagnostic
	switch {
	case SchemaHasTypeRef(schema):
		// A `{$type: X}` still standing means the reference did not resolve — a
		// missing type, a broken catalog, a types.yml that cannot be read — and
		// hashing what is left would stamp half a schema. So this half says it did
		// not run rather than inventing a mismatch on top of the real fault.
		//
		// An ERROR, not the warning the sibling `*_unchecked` codes carry. Those name
		// something missing from the INVOCATION — a `--modules` binding, a
		// `--service-name` — which the author may legitimately not have supplied, so
		// the run stays green and a repository's own gate greps for them. Here nothing
		// is missing from the invocation: every route to an unresolved `$type` is the
		// service contradicting itself, and one of those routes (a types.yml that
		// exists and cannot be read) is only a WARNING upstream. Green there would
		// mean an arbitrary `state_schema` edit reported by nothing at all.
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			File: lockPath, Code: "schema_lock_unchecked",
			Message: fmt.Sprintf("state_schema still holds an unresolved $type, so %s could not be checked against it", SchemaLockFile),
			Hint:    "fix the $type reference (or types.yml) first - the fingerprint is over the RESOLVED schema, and half a schema would hash to something meaningless; until then a state_schema edit is caught by nothing",
		})
	default:
		if current := FingerprintStateSchema(schema); current != lock.Fingerprint {
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				File: lockPath, Code: "schema_lock_stale",
				Message: fmt.Sprintf("state_schema does not match the stamp: stamped %s, current %s", lock.Fingerprint, current),
				Hint: fmt.Sprintf("state_schema was edited with no ladder step behind it - add %s/<NNN>_<slug>/ for the change and run `soul-lint schema-stamp <service dir>`; if the edit really needs no migration, re-stamp on its own and let the review see a lock change with no new step",
					MigrationsDirName),
			})
		}
	}
	// The version half reads two integers and no schema, so it runs under BOTH arms
	// above: a published step deleted off the top is not something to stop reporting
	// because a type reference elsewhere in the manifest went missing.
	if !diag.HasErrors(ladderDiags) && lock.Version != ladder.Version() {
		out = append(out, schemaLockVersionDiag(lockPath, lock.Version, ladder.Version()))
	}
	return out
}

// schemaLockVersionDiag reports a stamped version that is not the current top of the
// ladder. One code, two directions, because the two are different mistakes and the
// fix for one is not the fix for the other.
func schemaLockVersionDiag(lockPath string, stamped, current int) diag.Diagnostic {
	hint := fmt.Sprintf("a step was added without re-stamping - run `soul-lint schema-stamp <service dir>` and commit %s beside it", SchemaLockFile)
	if stamped > current {
		// The hole ADR-019's amendment left open and named this artifact as the
		// answer to. Deleting the top step lowers the derived version, and an
		// incarnation sitting at the NEW top then upgrades as a plain ref-bump: the
		// ladder and the incarnation agree, and only `state_schema` — which still
		// describes the shape the deleted step produced — does not. The stamp is the
		// one thing in the repository that remembers where the ladder used to end.
		hint = fmt.Sprintf("the ladder is SHORTER than when it was stamped - a published step was deleted, and every incarnation already at version %d now reads its own state as the shape a step that no longer exists produced; restore the step, or re-stamp deliberately if the ladder was never published",
			stamped)
	}
	return diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
		File: lockPath, Code: "schema_lock_version_stale",
		Message: fmt.Sprintf("stamped version %d, but the ladder tops out at %d", stamped, current),
		Hint:    hint,
	}
}

// StampSchemaLock writes the lock for a service whose schema and ladder have already
// been read and found sound. It returns the lock it wrote.
//
// `migrations/` is created when it does not exist: a service at version 1 has an
// empty ladder and a `state_schema` all the same, and the lock is specified to live
// beside the ladder whether or not the ladder has rungs yet.
func StampSchemaLock(serviceRoot string, schema InputSchemaMap, version int) (SchemaLock, error) {
	lock := SchemaLock{Version: version, Fingerprint: FingerprintStateSchema(schema)}
	path := SchemaLockPath(serviceRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return SchemaLock{}, err
	}
	if err := os.WriteFile(path, MarshalSchemaLock(lock), 0o644); err != nil {
		return SchemaLock{}, err
	}
	return lock, nil
}
