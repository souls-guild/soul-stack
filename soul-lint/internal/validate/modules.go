package validate

// `soul-lint --modules <alias>=<path>` — the offline half of the plugin-params check.
//
// Offline is the whole point: keeper resolves a plugin's contract from its Sigil
// grants, which an author writing a definition on their laptop does not have. Handed
// the schema documents instead, the author gets the same four checks keeper would run,
// before the run rather than as a `module.unknown_param` on the host.
//
// # Why the alias is on the flag
//
// The artifact carries no self-name (NIM-377). There is no `namespace:`, no `name:`,
// nothing in the bytes that says what a task should call it — address level 1 is the
// registration alias an operator picks, and the same artifact registered as `redis` and
// as `redis-community` serves two address spaces without changing a byte. So the
// linter cannot learn an address from a file; somebody has to state it.
//
// The alias goes on the flag rather than being taken from the directory. Directory
// naming was already rejected once for the manifest form, and every reason still holds:
// a plugin checkout is named after its binary (`soul-mod-redis`) or after
// whatever `git clone` produced, while a task addresses `redis.instance`. Reading the
// alias off the path would make a definition's address depend on where a file happens
// to sit — rename the folder and the same definition stops or starts validating, with
// nothing in the definition changed. `<alias>=<path>` keeps the address a STATED
// string: the same word the task writes and the same word the operator will register,
// checked against the same reserved list a registration is checked against.
//
// # Fail closed
//
// Every failure here is fatal (exit 2), not a downgrade to "unchecked". The author
// named this binding explicitly: an alias whose document cannot be read is a check they
// asked for and did not get, and running on to print `OK:` would be the exact failure
// the flag exists to remove. [config.DiagPluginParamsUnchecked] stays for the case it
// actually describes — a module nobody bound at all.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// aliasSchemas is a [config.ModuleManifestResolver] over the documents bound on the
// command line, keyed `<alias>.<module>`.
type aliasSchemas map[string]plugin.ModuleDef

func (a aliasSchemas) ResolveModule(alias, module string) (plugin.ModuleDef, bool) {
	m, ok := a[alias+"."+module]
	return m, ok
}

// LoadModuleSchemas builds the resolver from `<alias>=<path>` bindings.
//
// path is a published `schema.json`, a stamped artifact, or a directory holding the
// former — `dist/` after a build, which is what an author has in front of them.
//
// Nothing is skipped and nothing is best-effort: an unreadable, unparseable, invalid or
// wrong-kind document is an error for the whole run. That is the one real difference
// from the tree walk this replaced, and it follows from the flag's new shape — a tree
// could contain files that were none of the linter's business, whereas every binding
// here is one the author wrote down.
func LoadModuleSchemas(bindings []string) (config.ModuleManifestResolver, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	out := aliasSchemas{}
	boundTo := make(map[string]string, len(bindings))
	for _, b := range bindings {
		alias, path, err := parseModuleBinding(b)
		if err != nil {
			return nil, err
		}
		if prev, dup := boundTo[alias]; dup {
			// Two documents under one alias is not a merge — it is one address
			// space with two answers, and picking either silently would make the
			// lint depend on flag order.
			return nil, fmt.Errorf("--modules %s: alias %q is already bound to %s", b, alias, prev)
		}
		boundTo[alias] = path

		doc, err := readSchemaDocument(path)
		if err != nil {
			return nil, fmt.Errorf("--modules %s: %w", b, err)
		}
		if doc.Kind != plugin.KindSoulModule {
			return nil, fmt.Errorf("--modules %s: %s declares kind %q; only %q serves module states a task can address",
				b, path, doc.Kind, plugin.KindSoulModule)
		}
		for _, m := range doc.Modules {
			out[alias+"."+m.Name] = m
		}
	}
	return out, nil
}

// parseModuleBinding splits `<alias>=<path>` and vets the alias.
//
// The alias is checked against the same grammar and the same reserved list a
// registration is checked against ([plugin.ValidAlias], [plugin.IsReserved]). Binding
// `core=` here would let a document of the author's choosing define what `core.*`
// accepts, which is precisely what the reserved list exists to stop — and refusing it
// at the flag also tells an author early that the alias they had in mind will not
// survive `plugin.allow` either.
func parseModuleBinding(s string) (alias, path string, err error) {
	const form = "expected --modules <alias>=<path>, where <alias> is the name the task writes " +
		"(the `redis` in redis.acl.present) and <path> is a schema.json, a stamped artifact, or the dist/ dir holding one"

	alias, path, found := strings.Cut(s, "=")
	switch {
	case !found:
		// The artifact has no self-name, so there is no path from which the alias
		// could be inferred. Saying so beats accepting a bare directory and
		// indexing nothing.
		return "", "", fmt.Errorf("--modules %q: %s", s, form)
	case alias == "":
		return "", "", fmt.Errorf("--modules %q: empty alias; %s", s, form)
	case path == "":
		return "", "", fmt.Errorf("--modules %q: empty path; %s", s, form)
	case !plugin.ValidAlias(alias):
		return "", "", fmt.Errorf("--modules %q: alias %q does not match %s (lowercase kebab-case, starts with a letter, no dots)",
			s, alias, plugin.AliasPattern)
	case plugin.IsReserved(alias):
		return "", "", fmt.Errorf("--modules %q: alias %q is reserved and cannot be registered by any plugin; reserved: %s",
			s, alias, strings.Join(plugin.ReservedNames(), ", "))
	}
	return alias, path, nil
}

// readSchemaDocument reads one schema document from a file or a `dist/` directory.
//
// The two carriers are told apart by content, not by extension: a canonical document is
// a JSON object and so begins with `{`, an artifact does not. Guessing from the
// filename would misread `dist/soul-mod-redis.json` and, worse, would read an artifact
// named `schema.json` as text and report a parse error instead of a missing trailer.
func readSchemaDocument(path string) (*plugin.Document, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		path = filepath.Join(path, plugin.SchemaFileName)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("%w (a directory binding must hold %s, as `soul-mod stamp` writes it)", err, plugin.SchemaFileName)
		}
	}

	var (
		doc   *plugin.Document
		diags []diag.Diagnostic
	)
	if isJSONDocument(path) {
		doc, diags, err = plugin.ReadSchemaFile(path)
	} else {
		doc, diags, err = plugin.ReadArtifact(path)
	}
	if err != nil {
		return nil, err
	}
	if doc == nil || diag.HasErrors(diags) {
		if first := plugin.FirstError(diags); first != nil {
			return nil, first
		}
		return nil, fmt.Errorf("%s: no readable schema document", path)
	}
	return doc, nil
}

// isJSONDocument reports whether the file's first non-whitespace byte is `{`.
func isJSONDocument(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var head [64]byte
	n, _ := f.Read(head[:])
	trimmed := bytes.TrimLeft(head[:n], " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}
