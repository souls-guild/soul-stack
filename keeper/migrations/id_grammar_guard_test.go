package migrations

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/profile"
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

// The `name` -> `id` rename ([ADR-0085], NIM-729) makes one promise that nothing
// else in the tree checks: the identifier changed its SPELLING and not its
// GRAMMAR. Two copies of that grammar exist and they are enforced in different
// processes -- the Go constant, checked before the round trip so a malformed id
// gets a 422 instead of a wasted query, and the SQL CHECK, which is the actual
// floor. They agree today. Nothing made them keep agreeing.
//
// A drift between them is silent in both directions, and neither is caught by a
// compiler or by the OpenAPI guards next door, which pin the Go constant against
// the huma `pattern:` tag and stop at the process boundary:
//
//   - Go narrower than SQL: rows the database would accept are refused with a
//     422 that names a pattern the server does not actually enforce.
//   - Go wider than SQL: the insert reaches Postgres and comes back as a CHECK
//     violation, which the repository maps to a generic wrapped error rather
//     than to [serviceregistry.ErrInvalidID] -- so the caller gets a 500 where
//     the honest answer is a 422.
//
// This guard asserts the two literals are the same string, and deliberately NOT
// which string that is. [ADR-0085] decides one grammar for every registry
// (`^[a-z0-9][a-z0-9-]{0,62}$`) and adopting it is scheduled apart from the
// rename, because on most of these registries it is a NARROWING. That change
// will move both literals at once and this guard stays green through it -- a
// guard that pinned the value would have to be edited by the very change it
// exists to police, which is the same as not having it.
//
// For the same reason the SQL side is FOUND rather than declared: the grammar
// lives in whichever migration most recently established it, which is the CREATE
// TABLE today and would be a later `ADD CONSTRAINT` after any alteration. A
// guard that hard-coded "the CREATE migration" would go red on the first such
// change and be repaired by editing the guard -- again, the failure mode it is
// supposed to prevent.
//
// [ADR-0085]: ../../docs/adr/0085-entity-id-and-label.md

// renameMigration — the file NIM-729 renames the identifier columns in. One file
// for all ten entities, mirroring migration 117 which added `label` to the same
// ten; each batch of the rename appends its tables to it.
const renameMigration = "118_registry_id.up.sql"

// renameDownMigration — its inverse, checked with the same force as the up file.
// A `.down.sql` is reversed duplication, the most copy-prone artifact in this
// pattern, and it is exercised by nothing until somebody actually rolls back.
const renameDownMigration = "118_registry_id.down.sql"

// idGrammarCase — one registry's two copies of its identifier grammar.
type idGrammarCase struct {
	table      string // the renamed table; also the key the tree is scanned against
	oldCheck   string // the CHECK constraint's name before NIM-729
	newCheck   string // its name after
	goPattern  string // the exported Go constant
	goConstant string // where that constant lives, for the failure message
}

// idGrammarCases — one row per renamed registry. A batch of NIM-729 adds its row
// here in the same change that adds its `RENAME COLUMN` to the migration;
// [TestIDGrammar_EveryRenamedTableIsGuarded] is what makes forgetting loud, by
// reading the required set out of the migration rather than out of a constant
// somebody would have to remember to bump.
var idGrammarCases = []idGrammarCase{
	{
		table:      "service_registry",
		oldCheck:   "service_registry_name_format",
		newCheck:   "service_registry_id_format",
		goPattern:  serviceregistry.IDPattern,
		goConstant: "serviceregistry.IDPattern",
	},
	{
		table:      "providers",
		oldCheck:   "providers_name_format",
		newCheck:   "providers_id_format",
		goPattern:  provider.IDPattern,
		goConstant: "provider.IDPattern",
	},
	{
		table:      "profiles",
		oldCheck:   "profiles_name_format",
		newCheck:   "profiles_id_format",
		goPattern:  profile.IDPattern,
		goConstant: "profile.IDPattern",
	},
	{
		table:      "push_providers",
		oldCheck:   "push_providers_name_format",
		newCheck:   "push_providers_id_format",
		goPattern:  pushprovider.IDPattern,
		goConstant: "pushprovider.IDPattern",
	},
	{
		table:      "omens",
		oldCheck:   "omens_name_format",
		newCheck:   "omens_id_format",
		goPattern:  augur.IDPattern,
		goConstant: "augur.IDPattern",
	},
	{
		table:      "heralds",
		oldCheck:   "heralds_name_format",
		newCheck:   "heralds_id_format",
		goPattern:  herald.IDPattern,
		goConstant: "herald.IDPattern",
	},
	{
		table:      "tidings",
		oldCheck:   "tidings_name_format",
		newCheck:   "tidings_id_format",
		goPattern:  herald.IDPattern,
		goConstant: "herald.IDPattern (Herald and Tiding share one constant)",
	},
	{
		table:      "vigils",
		oldCheck:   "vigils_name_format",
		newCheck:   "vigils_id_format",
		goPattern:  oracle.IDPattern,
		goConstant: "oracle.IDPattern",
	},
	{
		table:      "decrees",
		oldCheck:   "decrees_name_format",
		newCheck:   "decrees_id_format",
		goPattern:  oracle.IDPattern,
		goConstant: "oracle.IDPattern (Vigil and Decree share one constant)",
	},
	{
		table:      "incarnation",
		oldCheck:   "incarnation_name_format",
		newCheck:   "incarnation_id_format",
		goPattern:  incarnation.IDPattern,
		goConstant: "incarnation.IDPattern",
	},
}

// checkDefinitionRe matches both shapes a CHECK on an identifier column is
// established in: inline in a CREATE TABLE (`CONSTRAINT x CHECK (col ~ '…')`)
// and as a later alteration (`ADD CONSTRAINT x CHECK (col ~ '…')`). Written
// against the shapes these migrations actually use rather than against SQL in
// general -- a parser that accepted more could only be wrong in more ways.
func checkDefinitionRe(constraint string) *regexp.Regexp {
	return regexp.MustCompile(
		`(?:ADD\s+)?CONSTRAINT\s+` + regexp.QuoteMeta(constraint) +
			`\s+CHECK\s*\(\s*\w+\s*~\s*'([^']*)'\s*\)`)
}

// findCHECKPattern returns the regex literal that is IN FORCE for the named
// constraint, plus the migration that established it: every `*.up.sql` is read
// in apply order and the last definition wins, under either the pre-rename or
// the post-rename constraint name. That is what the database ends up with, and
// it is why a later migration altering the grammar keeps this guard meaningful
// instead of breaking it.
func findCHECKPattern(files map[string]string, oldCheck, newCheck string) (pattern, from string, err error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names) // zero-padded NNN prefixes, so lexical order == apply order

	for _, n := range names {
		for _, constraint := range []string{oldCheck, newCheck} {
			if m := checkDefinitionRe(constraint).FindStringSubmatch(files[n]); m != nil {
				pattern, from = m[1], n
			}
		}
	}
	if from == "" {
		return "", "", fmt.Errorf(
			"no migration defines `CONSTRAINT %s|%s CHECK (<col> ~ '<pattern>')` — "+
				"either the constraint was renamed again or its CHECK is no longer a regex match",
			oldCheck, newCheck)
	}
	return pattern, from, nil
}

// verifyIDGrammar is the guard's whole judgement, as a pure function of the
// migration corpus and the Go constant — so the mutation test can feed it a
// mutated tree instead of editing files on disk.
func verifyIDGrammar(c idGrammarCase, files map[string]string, upSQL, downSQL string) error {
	sqlPattern, from, err := findCHECKPattern(files, c.oldCheck, c.newCheck)
	if err != nil {
		return fmt.Errorf("%s: %w", c.table, err)
	}
	if sqlPattern != c.goPattern {
		return fmt.Errorf(
			"%s: the identifier grammar has drifted between Go and SQL\n"+
				"  %s = %s\n"+
				"  CHECK in force (%s) = %s\n"+
				"the Go constant validates before the round trip and the CHECK is the floor; when they "+
				"disagree one of them is a lie to the caller (a 422 naming a rule the server does not "+
				"enforce, or a 500 where a 422 was the honest answer). Move BOTH, in one change.",
			c.table, c.goConstant, c.goPattern, from, sqlPattern)
	}

	// The up migration must rename, not restate.
	wantColumn := fmt.Sprintf("ALTER TABLE %s RENAME COLUMN name TO id;", c.table)
	if !strings.Contains(upSQL, wantColumn) {
		return fmt.Errorf("%s: %s is missing the column rename %q — that statement is the whole ticket, "+
			"and it is also what carries the CHECK predicate onto the new column name",
			c.table, renameMigration, wantColumn)
	}
	wantConstraint := fmt.Sprintf("ALTER TABLE %s RENAME CONSTRAINT %s TO %s;", c.table, c.oldCheck, c.newCheck)
	if !strings.Contains(upSQL, wantConstraint) {
		return fmt.Errorf(
			"%s: %s is missing the constraint rename %q\n"+
				"the constraint must be RENAMED, not dropped and re-added: a DROP + ADD retypes the "+
				"grammar by hand, leaves the table momentarily unconstrained, and runs a validation scan "+
				"that a row legal today can fail.",
			c.table, renameMigration, wantConstraint)
	}

	// And the down migration must undo exactly those two, or a rollback leaves a
	// table whose column and whose constraint name disagree about which rename
	// happened. Nothing else exercises this file.
	for _, want := range []string{
		fmt.Sprintf("ALTER TABLE %s RENAME COLUMN id TO name;", c.table),
		fmt.Sprintf("ALTER TABLE %s RENAME CONSTRAINT %s TO %s;", c.table, c.newCheck, c.oldCheck),
	} {
		if !strings.Contains(downSQL, want) {
			return fmt.Errorf("%s: %s is missing the inverse statement %q — a down migration that does "+
				"not fully reverse its up is found only by an operator mid-rollback",
				c.table, renameDownMigration, want)
		}
	}
	return nil
}

// migrationCorpus reads every `*.up.sql` out of the embedded FS.
func migrationCorpus(t *testing.T) map[string]string {
	t.Helper()
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the migration FS: %v", err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		b, err := FS.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("reading migration %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("the migration FS produced no `.up.sql` files — the guard would pass vacuously")
	}
	return out
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := FS.ReadFile(name)
	if err != nil {
		t.Fatalf("reading migration %s: %v", name, err)
	}
	return string(b)
}

// renamedTables returns every table the rename migration converts, read out of
// the migration itself.
var renameStmtRe = regexp.MustCompile(`ALTER TABLE (\w+) RENAME COLUMN name TO id;`)

func renamedTables(upSQL string) []string {
	var out []string
	for _, m := range renameStmtRe.FindAllStringSubmatch(upSQL, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// TestIDGrammar_GoConstantMatchesTheCHECK — the guard itself, over the real
// migrations and the real constants.
func TestIDGrammar_GoConstantMatchesTheCHECK(t *testing.T) {
	files := migrationCorpus(t)
	upSQL := readMigration(t, renameMigration)
	downSQL := readMigration(t, renameDownMigration)

	for _, c := range idGrammarCases {
		t.Run(c.table, func(t *testing.T) {
			if err := verifyIDGrammar(c, files, upSQL, downSQL); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestIDGrammar_EveryRenamedTableIsGuarded closes the hole a hand-maintained
// count would leave open.
//
// The question is "does every table the migration renames have a grammar check",
// and the only honest source for the left-hand side is the migration. A constant
// compared against `len(idGrammarCases)` would have answered a different
// question — it can only fire when somebody ADDS a row and forgets to bump it,
// i.e. it punishes the correct action and stays silent on the incorrect one.
func TestIDGrammar_EveryRenamedTableIsGuarded(t *testing.T) {
	upSQL := readMigration(t, renameMigration)

	guarded := make(map[string]bool, len(idGrammarCases))
	for _, c := range idGrammarCases {
		guarded[c.table] = true
	}

	renamed := renamedTables(upSQL)
	if len(renamed) == 0 {
		t.Fatalf("%s renames no column — either the migration was gutted or the statement shape moved, "+
			"and either way every case below would pass vacuously", renameMigration)
	}
	for _, table := range renamed {
		if !guarded[table] {
			t.Errorf("%s renames %s.name -> .id, but idGrammarCases has no row for it: its Go constant "+
				"and its SQL CHECK are free to drift apart unnoticed. Add the row in the same change "+
				"that renames the table.", renameMigration, table)
		}
		delete(guarded, table)
	}
	for table := range guarded {
		t.Errorf("idGrammarCases has a row for %s, which %s does not rename — the case is checking a "+
			"grammar against a migration that never ran", table, renameMigration)
	}
}

// TestIDGrammar_GuardCatchesDrift is the mutation half: it feeds
// [verifyIDGrammar] trees that are wrong in each of the ways the guard exists to
// catch, and fails if the guard passes them.
//
// The mutations are real SQL and real pattern strings, not sentinel markers —
// each is what the corresponding mistake would actually look like in the tree,
// so a guard that only recognised a marker would not survive this. Each case
// asserts on a message fragment unique to the failure it induces, so a case that
// passed for the wrong reason still fails.
func TestIDGrammar_GuardCatchesDrift(t *testing.T) {
	base := idGrammarCases[0]
	files := migrationCorpus(t)
	upSQL := readMigration(t, renameMigration)
	downSQL := readMigration(t, renameDownMigration)

	// Sanity: the unmutated inputs pass, so a failure below is the mutation and
	// not a broken fixture.
	if err := verifyIDGrammar(base, files, upSQL, downSQL); err != nil {
		t.Fatalf("the unmutated tree does not pass the guard, so this test proves nothing: %v", err)
	}
	_, definedIn, err := findCHECKPattern(files, base.oldCheck, base.newCheck)
	if err != nil {
		t.Fatalf("locating the CHECK in force: %v", err)
	}

	tests := []struct {
		name      string
		mutate    func(c *idGrammarCase, files map[string]string, up, down *string)
		wantMatch string
	}{
		{
			// The exact change this ticket declined to make silently: adopt
			// ADR-0085's unified grammar in Go and leave the CHECK behind.
			name: "Go constant narrowed without the migration",
			mutate: func(c *idGrammarCase, _ map[string]string, _, _ *string) {
				c.goPattern = `^[a-z0-9][a-z0-9-]{0,62}$`
			},
			wantMatch: "drifted between Go and SQL",
		},
		{
			// The mirror: the SQL floor moves and the Go constant does not, so
			// the server accepts what the database refuses and the caller gets a
			// 500 for a validation error. Derived from the case rather than
			// hard-coded, so the mutation follows whatever grammar is in force.
			name: "CHECK widened without the Go constant",
			mutate: func(c *idGrammarCase, files map[string]string, _, _ *string) {
				files[definedIn] = strings.Replace(files[definedIn],
					"'"+c.goPattern+"'", `'^[A-Za-z][A-Za-z0-9-]*$'`, 1)
			},
			wantMatch: "drifted between Go and SQL",
		},
		{
			// A LATER migration alters the CHECK and the Go constant does not
			// follow. This is the case a guard anchored on the CREATE migration
			// could not see at all — it would keep comparing against the stale
			// original and report everything fine.
			name: "a later migration alters the CHECK, Go does not follow",
			mutate: func(c *idGrammarCase, files map[string]string, _, _ *string) {
				files["999_alter_id_grammar.up.sql"] = fmt.Sprintf(
					"ALTER TABLE %s DROP CONSTRAINT %s;\n"+
						"ALTER TABLE %s ADD CONSTRAINT %s CHECK (id ~ '^[a-z0-9][a-z0-9-]{0,62}$');\n",
					c.table, c.newCheck, c.table, c.newCheck)
			},
			wantMatch: "drifted between Go and SQL",
		},
		{
			// A batch author who re-creates the constraint instead of renaming
			// it: the same end state on an empty database, a failed migration
			// and a retyped predicate on a populated one.
			name: "constraint dropped and re-added instead of renamed",
			mutate: func(c *idGrammarCase, _ map[string]string, up, _ *string) {
				*up = strings.Replace(*up,
					fmt.Sprintf("ALTER TABLE %s RENAME CONSTRAINT %s TO %s;", c.table, c.oldCheck, c.newCheck),
					fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s;\n"+
						"ALTER TABLE %s ADD CONSTRAINT %s CHECK (id ~ '%s');",
						c.table, c.oldCheck, c.table, c.newCheck, c.goPattern), 1)
			},
			wantMatch: "must be RENAMED",
		},
		{
			// The rename migration that forgets the column and moves only the
			// constraint — which would leave every query in the repository
			// selecting a column still called `name`.
			name: "column rename dropped from the up migration",
			mutate: func(c *idGrammarCase, _ map[string]string, up, _ *string) {
				*up = strings.Replace(*up,
					fmt.Sprintf("ALTER TABLE %s RENAME COLUMN name TO id;", c.table), "", 1)
			},
			wantMatch: "is missing the column rename",
		},
		{
			// The copy-prone one: the down migration reverses the column and
			// forgets the constraint, so a rollback leaves the two disagreeing.
			name: "down migration forgets the constraint rename",
			mutate: func(c *idGrammarCase, _ map[string]string, _, down *string) {
				*down = strings.Replace(*down,
					fmt.Sprintf("ALTER TABLE %s RENAME CONSTRAINT %s TO %s;", c.table, c.newCheck, c.oldCheck), "", 1)
			},
			wantMatch: "is missing the inverse statement",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, up, down := base, upSQL, downSQL
			mutFiles := make(map[string]string, len(files))
			for k, v := range files {
				mutFiles[k] = v
			}
			tc.mutate(&c, mutFiles, &up, &down)

			// The mutation has to actually change something, or the case would
			// be asserting on the untouched tree.
			if c == base && up == upSQL && down == downSQL && sameCorpus(mutFiles, files) {
				t.Fatal("the mutation changed nothing — the case asserts on the unmutated tree")
			}

			err := verifyIDGrammar(c, mutFiles, up, down)
			if err == nil {
				t.Fatalf("the guard accepted a mutated tree; it would not catch this in review either")
			}
			if !strings.Contains(err.Error(), tc.wantMatch) {
				t.Errorf("the guard rejected the mutation for the wrong reason:\n  got %v\n  want a message containing %q",
					err, tc.wantMatch)
			}
		})
	}
}

func sameCorpus(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
