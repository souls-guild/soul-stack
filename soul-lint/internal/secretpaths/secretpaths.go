// Package secretpaths implements the `soul-lint list-secret-paths` subcommand: the
// Vault address of every secret a service declares, printed from the declarations
// themselves.
//
// The address is derived and never authored ([ADR-0083] §1). That is deliberate — a
// path written in the service repository would be a second source of truth, and
// reconciling two copies of one path is the class of bug that ADR exists to remove.
// The cost is that an author cannot read off where their secret lands: until this
// command the only way to know was to read [config.SecretField.VaultPath]. The
// precedent for showing a derived thing is `passage_plan`, which prints a run order
// that is likewise written nowhere.
//
// What is printed is the SHAPE of the path, not a path. Two segments cannot be known
// offline and stay placeholders:
//
//   - `<incarnation>` — there is no incarnation at authoring time, and the linter never
//     talks to a keeper.
//   - the last segment of a COLLECTION — it comes from state DATA, an operator-supplied
//     element name. The declaration only says which sibling property supplies it, so
//     `key: name` prints as `<name>`: the key's NAME, not any value it will take.
//
// The service segment is the third thing no file in the repository states (NIM-726) —
// hence `--service-name`, the same flag and the same reason as `validate-scenario`.
//
// The trailing `#<field>` is the field inside the KV entry, in the form ADR-0083 §1
// writes it. A collection secret is keyed by its own property name; a scalar one has no
// property name to use and lands under [config.ScalarSecretVaultField]. Without it the
// output would name a KV entry and leave the reader to guess which key inside it holds
// the value.
package secretpaths

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Exit codes are a stable CLI contract (see soul-lint/cmd/soul-lint/main.go).
const (
	ExitOK        = 0
	ExitHasErrors = 1
	ExitIOFatal   = 2
)

// Options — parameters for one `list-secret-paths` run.
type Options struct {
	// Path is the service.yml to read the declarations from.
	Path string
	// ServiceName is the name the service is REGISTERED under — the second segment of
	// every derived path. Required: with it empty there is no path to print, and a
	// guessed one would be wrong in the one place the reader came here to check.
	ServiceName string
}

// The placeholders are not valid Vault path segments — `<` and `>` sit outside the
// [config.ValidVaultPathSegment] grammar — so they cannot be handed to
// [config.SecretField.VaultPath] directly. The shape is therefore derived through the
// REAL derivation with sentinel segments, and the sentinels are substituted afterwards.
// Formatting the shape by hand here instead would put a second copy of the formula
// inside the tool whose whole purpose is to explain the first one.
const (
	incarnationSentinel = "SoulLintIncarnationSentinel"
	keySentinel         = "SoulLintKeySentinel"
)

// Run prints one line per declared secret and returns the exit code.
func Run(opts Options, out, errOut io.Writer) int {
	if opts.ServiceName == "" {
		fmt.Fprintln(errOut, "soul-lint list-secret-paths: --service-name is required — the service segment of every derived path comes from it, and no file in a service repository states the name (NIM-726)")
		return ExitIOFatal
	}

	src, err := os.ReadFile(opts.Path)
	if err != nil {
		fmt.Fprintf(errOut, "soul-lint: %s: %v\n", opts.Path, err)
		return ExitIOFatal
	}
	svc, _, diags, _ := config.LoadServiceManifestFromBytes(opts.Path, src, config.ValidateOptions{})
	// An empty list is an ANSWER — "this service declares no secrets" — so it may only
	// be printed when the declarations were actually read. Two ways they were not, and
	// both are refusals rather than an empty list:
	//
	//   - the file did not parse at all;
	//   - it parsed, but carries no `state_schema` map. That is every YAML in a service
	//     repository which is not the manifest — types.yml, covenant.yml, a scenario's
	//     main.yml — and a manifest whose `state_schema:` is a scalar or a list. Pointed
	//     at one of those, a command that printed nothing and exited 0 would answer a
	//     question it was never able to ask.
	//
	// Errors from the later phases are left alone: they do not make `state_schema`
	// unreadable, and validate-service is where they belong. The check is deliberately
	// the presence of the map rather than the `state_schema_root_not_object` diagnostic
	// — that one rests on `type: object`, which the input dialect removes (NIM-742).
	//
	// A schema that is a map but malformed BELOW the root needs no check here:
	// [config.CollectSecretFields] scans the whole tree for `type: secret` first and
	// reports every node the two supported shapes do not claim, so a declaration hidden
	// under a broken sub-node comes back as a refusal, not as silence.
	switch {
	case svc == nil || hasParseError(diags):
		fmt.Fprintf(errOut, "soul-lint list-secret-paths: %s does not parse — run `soul-lint validate-service %s`\n", opts.Path, opts.Path)
		return ExitHasErrors
	case svc.StateSchema == nil:
		fmt.Fprintf(errOut, "soul-lint list-secret-paths: %s carries no `state_schema:` map — refusing to report \"no declared secrets\" about a file whose declarations were never read (is this the service.yml?)\n", opts.Path)
		return ExitHasErrors
	}

	// The traversal order is [config.CollectSecretFields]'s, which sorts. Reusing it
	// rather than walking the schema here is what keeps this command working when
	// `state_schema` moves to the input dialect (NIM-742), and what keeps one run
	// byte-identical to the next.
	fields, issues := config.CollectSecretFields(svc.StateSchema)

	type line struct{ form, decl string }
	var (
		lines    []line
		failures []string
		width    int
	)
	for _, f := range fields {
		form, err := pathForm(f, opts.ServiceName)
		if err != nil {
			failures = append(failures, "soul-lint list-secret-paths: "+err.Error())
			continue
		}
		if w := utf8.RuneCountInString(form); w > width {
			width = w
		}
		lines = append(lines, line{form: form, decl: declarationRef(f)})
	}
	for _, l := range lines {
		fmt.Fprintf(out, "%s   ← %s\n", l.form+strings.Repeat(" ", width-utf8.RuneCountInString(l.form)), l.decl)
	}

	for _, iss := range issues {
		fmt.Fprintf(errOut, "%s: %s: %s\n", iss.Path, iss.Code, iss.Message)
	}
	for _, f := range failures {
		fmt.Fprintln(errOut, f)
	}
	if len(issues) > 0 || len(failures) > 0 {
		// Said out loud rather than left to the exit code: a reader who scrolled past
		// stderr would otherwise take a short list for the whole list, which is the one
		// wrong belief this command can produce.
		fmt.Fprintln(errOut, "soul-lint list-secret-paths: the list above is INCOMPLETE — a refused declaration derives no path")
		return ExitHasErrors
	}
	return ExitOK
}

// pathForm renders one declaration's derived address with the two unknowable segments
// left as placeholders. The mount is the default one: keeper.yml is not a file of the
// service repository, so an offline linter has no configured mount to read
// ([config.EffectiveVaultMount] resolves the empty string to it).
func pathForm(f config.SecretField, service string) (string, error) {
	// The key's NAME is printed as the placeholder, and nothing upstream checks it:
	// [config.CollectSecretFields] validates the state field and the property because
	// those become segments, and it validates the key's runtime VALUE at derivation
	// time — but the key's own name never reaches a path, so it is never checked. Here
	// it does reach one. A property named `a/b` would render `<a/b>`, which reads as two
	// segments where the derivation makes one, and `a#b` would collide with the field
	// suffix. Refuse rather than print a shape whose punctuation lies about the
	// structure.
	if f.Collection() && !config.ValidVaultPathSegment(f.Key) {
		return "", fmt.Errorf("secret field %s: key names the property %q, which is not a safe Vault path segment — its name is printed as the path's last segment and would read as punctuation",
			f.ID(), f.Key)
	}
	path, err := f.VaultPath("", service, incarnationSentinel, keySentinel)
	if err != nil {
		return "", err
	}
	segs := strings.Split(path, "/")
	var incarnations, keys int
	for i, s := range segs {
		switch s {
		case incarnationSentinel:
			segs[i] = "<incarnation>"
			incarnations++
		case keySentinel:
			segs[i] = "<" + f.Key + ">"
			keys++
		}
	}
	wantKeys := 0
	if f.Collection() {
		wantKeys = 1
	}
	// Fail closed rather than print a shape that does not match what the derivation
	// produced. This catches both halves of the substitution going wrong: a formula that
	// stopped placing the incarnation where this code expects it, and a service or state
	// field name that collides with a sentinel and would have been rewritten as a
	// placeholder.
	if incarnations != 1 || keys != wantKeys {
		return "", fmt.Errorf("secret field %s: derived path %q carries %d incarnation segment(s) and %d key segment(s), want 1 and %d — refusing to print a shape that disagrees with the derivation",
			f.ID(), path, incarnations, keys, wantKeys)
	}
	// The `#` join is [config.SecretField.VaultRef]'s, minus the `vault:` scheme that
	// makes it a reference rather than an address. It is spelled out rather than
	// trimmed off VaultRef's result because the scheme has no exported constant, and
	// trimming a hard-coded prefix would re-encode more than this does.
	return strings.Join(segs, "/") + "#" + f.VaultField(), nil
}

// declarationRef names the declaration a line came from, in the terms the author wrote
// it: the state field, and for a collection the property inside the element.
//
// Built from State/Property and not from [config.SecretField.Path], which spells the
// JSON-Schema route (`.properties.redis_users.items.properties.password`) and changes
// shape when `state_schema` moves to the input dialect (NIM-742).
func declarationRef(f config.SecretField) string {
	if f.Collection() {
		return "state_schema." + f.State + "[]." + f.Property
	}
	return "state_schema." + f.State
}

func hasParseError(ds []diag.Diagnostic) bool {
	for _, d := range ds {
		if d.Level == diag.LevelError && d.Phase == diag.PhaseParse {
			return true
		}
	}
	return false
}
