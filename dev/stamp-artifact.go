// Command stamp-artifact appends a plugin's published schema document to a built
// artifact as a trailer — what `soul-mod stamp` does, with the document supplied
// rather than derived.
//
// `soul-mod stamp` asks the artifact for its own schema by running it
// (`<artifact> schema`), and dev/provision.sh cannot: it cross-compiles the plugin
// for linux/amd64 on whatever the dev machine is, and the reference plugin serves
// through module.Serve, which has no `schema` subcommand at all. So the bytes come
// from the document the repository publishes beside the plugin's sources — the same
// file tests/e2e-live/harness/plugin.go stamps its fixture with, and the one
// `make lint` validates. Keeper reads that document by seeking from the end WITHOUT
// executing the artifact (at plugin.allow it is not approved yet), so an unstamped
// binary has no disclosure and every reader fails closed.
//
// It is a `go run` file outside the workspace modules, and it exists at all because
// the trailer format is defined once, in sdk/schema: shell cannot append it without
// writing a second copy of the wire format that would go on agreeing with the old
// one after the SDK moved.
//
//	go run dev/stamp-artifact.go <artifact> <document>
package main

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run dev/stamp-artifact.go <artifact> <document>")
		os.Exit(2)
	}
	artifact, documentPath := os.Args[1], os.Args[2]

	document, err := os.ReadFile(documentPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stamp-artifact: read %s: %v\n", documentPath, err)
		os.Exit(1)
	}
	// Refuse anything a reader downstream would refuse, in the order soul-mod's own
	// derivedDocument refuses it: parse, validate, then canonical form. Stamping stores
	// these bytes verbatim, so a document keeper would reject must not reach the trailer
	// — it resolves to a per-entry warning and a stand that comes up looking healthy with
	// the plugin silently absent.
	doc, err := schema.Unmarshal(document)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stamp-artifact: %s: %v\n", documentPath, err)
		os.Exit(1)
	}
	if issues := schema.Validate(doc); schema.HasErrors(issues) {
		fmt.Fprintf(os.Stderr, "stamp-artifact: %s is invalid:\n", documentPath)
		for _, i := range issues {
			if i.Level == schema.LevelError {
				fmt.Fprintf(os.Stderr, "  %s %s: %s\n", i.Path, i.Code, i.Message)
			}
		}
		os.Exit(1)
	}
	// Canonicality is what the signature depends on (ADR-026): the bytes are hashed,
	// and a reformatted copy is a different artifact. Refusing here makes a hand-edited
	// document a provisioning failure with the file named, instead of a keeper-side
	// verify mismatch three steps into a live run.
	canonical, err := schema.IsCanonical(document)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stamp-artifact: %s: %v\n", documentPath, err)
		os.Exit(1)
	}
	if !canonical {
		fmt.Fprintf(os.Stderr, "stamp-artifact: %s is not in canonical form — regenerate it, do not hand-edit\n", documentPath)
		os.Exit(1)
	}
	if err := schema.WriteTrailerFile(artifact, document); err != nil {
		fmt.Fprintf(os.Stderr, "stamp-artifact: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("stamped %s with %s (%d bytes)\n", artifact, documentPath, len(document))
}
