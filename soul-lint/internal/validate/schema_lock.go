package validate

// The two halves of the schema lock ([ADR-019] clause 4, NIM-737): the check that
// runs on every `validate-service` (and so on every `validate-service-tree`, which
// walks the manifest through the same check set), and the `schema-stamp` command
// that writes the file.
//
// They live in ONE file on purpose. The stamp writes what the check compares, so a
// difference of a single step between how the two read the schema makes every lock
// in existence wrong — a green stamp followed by a red validate, with nothing in
// either message able to say why. Both therefore reach the schema the same way:
// parse the manifest, then [stateSchemaTypeRefDiags], which resolves `$type` in
// place and is the only resolver either path has. The same argument NIM-736 made for
// `ScanMigrationLadder` being the one reader of the ladder, one level up.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// schemaLockDiags compares `migrations/schema.lock` against the manifest.
//
// svc is the manifest as [diagnose] has it — already `$type`-resolved in place by
// [stateSchemaTypeRefDiags], which must therefore run before this. A nil svc means
// the manifest did not parse at all: there is no schema to fingerprint, and the
// author has the parse error.
func schemaLockDiags(servicePath string, svc *config.ServiceManifest) []diag.Diagnostic {
	if svc == nil {
		return nil
	}
	return config.ValidateSchemaLock(filepath.Dir(servicePath), svc.StateSchema)
}

// StampOptions holds the parameters of one `schema-stamp` run.
type StampOptions struct {
	// Root is the service directory, or the `service.yml` inside it — the same
	// positional `validate-service-tree` takes, resolved by the same function.
	Root string
}

// RunStamp writes `migrations/schema.lock` for a service, and returns an exit code
// on the same contract as [Run]: 0 = stamped, 1 = refused because the service is
// red, 2 = the caller is wrong (this is not a service tree, the manifest cannot be
// read).
//
// **It refuses to stamp when the MANIFEST or the LADDER is red, and that is the
// point.** A stamp is a claim that this schema and this ladder were, at one moment,
// both sound and in agreement. Generated from a manifest that does not parse, or
// over a ladder with a gap in it, the file would freeze a version nobody meant and a
// fingerprint of half a document — and it would then MATCH, silently, for as long as
// nobody touched the schema again. A generator that runs on a broken subject does
// not produce a partial artifact; it produces a confident wrong one.
//
// Those two and no more: a broken scenario, a malformed `vars/_stack.yaml` or a
// failing compat floor still stamps, because none of them is evidence about the
// shape of `incarnation.state`. Refusing on them would make an unrelated red part of
// the repository block a correct stamp, and the author would reach for the one
// escape a generator must not have — writing the file by hand.
//
// What it does NOT refuse is a re-stamp with no new step behind it. That bypass is
// deliberate and it is closed procedurally rather than mechanically ([ADR-019]
// clause 4): re-stamping shows up in the diff as a changed `schema.lock` with no new
// directory under `migrations/`, and the reviewer reads it there. The mechanical ban
// was considered and rejected — it would force an empty migration step for every
// harmless schema edit, and a ladder padded with no-op rungs costs more than the
// two-line diff it replaces.
func RunStamp(opts StampOptions, out io.Writer, errOut io.Writer) int {
	root, err := serviceTreeRoot(opts.Root)
	if err != nil {
		fmt.Fprintf(errOut, "soul-lint schema-stamp: %v\n", err)
		return ExitIOFatal
	}
	manifestPath := filepath.Join(root, serviceManifestFile)
	src, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		fmt.Fprintf(errOut, "soul-lint schema-stamp: %s: %v\n", manifestPath, readErr)
		return ExitIOFatal
	}

	svc, diags := stateSchemaResolvedForLock(manifestPath, src)
	// The ladder is read strictly here, exactly as `validate-service` reads it: the
	// version being stamped is the top of it, so a ladder with a gap or a duplicate
	// is a ladder whose top means nothing yet.
	diags = append(diags, config.ValidateMigrationLadder(root)...)
	for _, d := range diags {
		writeHumanDiag(errOut, d)
	}
	if svc == nil || diag.HasErrors(diags) {
		fmt.Fprintf(errOut, "soul-lint schema-stamp: refusing to stamp %s - its manifest or its ladder is red, and a stamp says the schema and the ladder agreed\n", root)
		return ExitHasErrors
	}
	if config.SchemaHasTypeRef(svc.StateSchema) {
		// Reachable without an error above: an unreadable types.yml is a WARNING,
		// and the references then stand unresolved. Hashing them would stamp half a
		// schema, and half a schema goes on matching while the named type changes
		// underneath it.
		fmt.Fprintf(errOut, "soul-lint schema-stamp: refusing to stamp %s - state_schema still holds an unresolved $type, and the fingerprint is over the RESOLVED schema\n", root)
		return ExitHasErrors
	}

	ladder, _ := config.ScanMigrationLadder(root)
	lock, stampErr := config.StampSchemaLock(root, svc.StateSchema, ladder.Version())
	if stampErr != nil {
		fmt.Fprintf(errOut, "soul-lint schema-stamp: %v\n", stampErr)
		return ExitIOFatal
	}
	fmt.Fprintf(out, "stamped %s: version %d, fingerprint %s\n", config.SchemaLockPath(root), lock.Version, lock.Fingerprint)
	return ExitOK
}

// stateSchemaResolvedForLock parses a service manifest and resolves the `$type`
// references of its `state_schema` in place — the state the schema has to be in
// before it is fingerprinted, and the state [diagnose] leaves it in for the check.
//
// A nil manifest back means the document did not parse; the diagnostics say so.
func stateSchemaResolvedForLock(manifestPath string, src []byte) (*config.ServiceManifest, []diag.Diagnostic) {
	svc, _, diags, _ := config.LoadServiceManifestFromBytes(manifestPath, src, config.ValidateOptions{})
	return svc, append(diags, stateSchemaTypeRefDiags(manifestPath, svc)...)
}
