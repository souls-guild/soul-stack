// Command soul-mod is the module author's build-time tool: it stamps an artifact's
// schema into the artifact, and verifies that the stamp still matches the code.
//
//	build:
//		go build -trimpath -ldflags="-s -w" -o dist/soul-mod-redis ./cmd/soul-mod-redis
//		soul-mod stamp dist/soul-mod-redis        # schema into the artifact + dist/schema.json
//
//	check:
//		soul-mod verify dist/soul-mod-redis       # stamped schema == code schema
//
// # This is not a fourth Soul Stack binary
//
// ADR-004 fixes the product at three artifacts — `keeper`, `soul`, `soul-lint` — and
// that is untouched. soul-mod ships in the Apache-2.0 `sdk/` module for people writing
// plugins; it runs on an author's machine and in an author's CI, is never installed on
// a Keeper or a Soul host, and is not part of any Soul Stack distribution package. It
// is a build tool in the same sense `protoc-gen-go` is, not an operator interface.
//
// # What stamp does
//
// It runs the artifact's own `schema` subcommand — the artifact is the source of truth
// about itself — checks that what came back is a valid, canonical document, and appends
// it as a trailer (magic + length + payload after the ELF, see `sdk/schema`). Loaders
// ignore trailing bytes, so the artifact stays runnable; Keeper reads the document by
// seeking from the end, WITHOUT executing anything, which is the point: at
// `plugin.allow` the binary is not yet approved.
//
// The same bytes go to `schema.json` next to the artifact, for `soul-lint`, which
// should not have to download a binary to check a destiny.
//
// # What verify does
//
// It re-derives the document from the artifact and compares it byte for byte against
// what is stamped, and against `schema.json` when that file is there. A mismatch means
// someone changed a [module.Def] and shipped a stale stamp — the build must fail, not
// warn, because everything downstream trusts the stamp over the code it can no longer
// see.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, execSchema))
}

// deriver returns an artifact's own view of its schema. The production
// implementation runs the artifact; tests substitute a function so the logic under
// test is the stamping, not the forking.
type deriver func(artifact string) ([]byte, error)

const usage = `usage:
  soul-mod stamp  <artifact>   write the artifact's schema into it and to schema.json
  soul-mod verify <artifact>   check the stamped schema still matches the code
`

// Exit codes: 0 success, 1 the artifact failed its check, 2 the command was wrong.
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	schemaPerms = 0o644
)

func run(args []string, stdout, stderr io.Writer, derive deriver) int {
	if len(args) != 2 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, artifact := args[0], args[1]
	switch cmd {
	case "stamp":
		if err := stamp(artifact, stdout, derive); err != nil {
			fmt.Fprintf(stderr, "soul-mod stamp: %v\n", err)
			return exitFailed
		}
		return exitOK
	case "verify":
		if err := verify(artifact, stdout, derive); err != nil {
			fmt.Fprintf(stderr, "soul-mod verify: %v\n", err)
			return exitFailed
		}
		return exitOK
	default:
		fmt.Fprintf(stderr, "soul-mod: unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
}

func stamp(artifact string, stdout io.Writer, derive deriver) error {
	payload, err := derivedDocument(artifact, derive)
	if err != nil {
		return err
	}
	if err := schema.WriteTrailerFile(artifact, payload); err != nil {
		return err
	}
	jsonPath := schemaFilePath(artifact)
	if err := os.WriteFile(jsonPath, payload, schemaPerms); err != nil {
		return fmt.Errorf("write %s: %w", jsonPath, err)
	}
	fmt.Fprintf(stdout, "stamped %s (%d bytes) and wrote %s\n", artifact, len(payload), jsonPath)
	return nil
}

func verify(artifact string, stdout io.Writer, derive deriver) error {
	stamped, err := schema.ReadTrailerFile(artifact)
	if err != nil {
		return err
	}
	fromCode, err := derivedDocument(artifact, derive)
	if err != nil {
		return err
	}
	if !bytes.Equal(stamped, fromCode) {
		return fmt.Errorf("%s: the stamped schema does not match the code\n  stamped: %s\n  code:    %s\n"+
			"re-run `soul-mod stamp %s` after changing a module.Def",
			artifact, preview(stamped), preview(fromCode), artifact)
	}
	jsonPath := schemaFilePath(artifact)
	published, err := os.ReadFile(jsonPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(stdout, "verified %s (%d bytes); %s is absent\n", artifact, len(stamped), jsonPath)
		return nil
	case err != nil:
		return fmt.Errorf("read %s: %w", jsonPath, err)
	case !bytes.Equal(published, fromCode):
		return fmt.Errorf("%s does not match the code (the artifact and its published schema disagree); "+
			"re-run `soul-mod stamp %s`", jsonPath, artifact)
	}
	fmt.Fprintf(stdout, "verified %s and %s (%d bytes)\n", artifact, jsonPath, len(stamped))
	return nil
}

// derivedDocument asks the artifact for its schema and refuses anything a reader
// downstream would refuse: unparseable, invalid, or not in canonical form. Stamping
// stores these bytes verbatim, so they have to be bytes this SDK could have produced
// itself — otherwise `verify` would be comparing against a moving target.
func derivedDocument(artifact string, derive deriver) ([]byte, error) {
	raw, err := derive(artifact)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s: the schema subcommand printed nothing", artifact)
	}
	doc, err := schema.Unmarshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", artifact, err)
	}
	if issues := schema.Validate(doc); schema.HasErrors(issues) {
		var b bytes.Buffer
		fmt.Fprintf(&b, "%s: the schema is invalid", artifact)
		for _, i := range issues {
			if i.Level != schema.LevelError {
				continue
			}
			fmt.Fprintf(&b, "\n  %s %s: %s", i.Path, i.Code, i.Message)
		}
		return nil, errors.New(b.String())
	}
	canonical, err := schema.IsCanonical(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", artifact, err)
	}
	if !canonical {
		return nil, fmt.Errorf("%s: the schema subcommand did not print canonical JSON "+
			"(build the artifact against this SDK version)", artifact)
	}
	return raw, nil
}

// execSchema runs `<artifact> schema` and returns its stdout. stderr is folded into
// the error so an author sees why their own binary refused.
func execSchema(artifact string) ([]byte, error) {
	abs, err := filepath.Abs(artifact)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", artifact, err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(abs, schema.SchemaSubcommand)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) > 0 {
			return nil, fmt.Errorf("run %s %s: %w: %s", artifact, schema.SchemaSubcommand, err, msg)
		}
		return nil, fmt.Errorf("run %s %s: %w", artifact, schema.SchemaSubcommand, err)
	}
	return stdout.Bytes(), nil
}

func schemaFilePath(artifact string) string {
	return filepath.Join(filepath.Dir(artifact), schema.SchemaFileName)
}

// preview trims a document to something that fits in an error message while still
// showing where two documents diverge.
func preview(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}
