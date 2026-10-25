package operator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// namesTableInSQL reports whether a string literal of the file names table.
// Only literals count: SQL lives in them, and a comment that says where the
// hash lives reads nothing.
func namesTableInSQL(t *testing.T, path, table string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, table) {
			found = true
		}
		return !found
	})
	return found
}

// The local-password hash never leaves its table (NIM-1051): no read of an
// operator carries it, no other package queries it. The guard is the table
// name — any SQL that reads the hash has to name `operator_passwords` — so it
// may appear in a Go string literal only in the one file that owns it. A JOIN added to
// an operator read, a second repository in another package, a "convenience"
// query in a handler all name it, and all turn this red.
func TestOperatorPasswords_TableIsReadOnlyByItsOwner(t *testing.T) {
	const owner = "internal/operator/password.go"
	root, err := filepath.Abs("../..") // keeper/
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "migrations" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == owner {
			return nil
		}
		if namesTableInSQL(t, path, "operator_passwords") {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(offenders) > 0 {
		t.Fatalf("operator_passwords is named outside %s: %v\n"+
			"the table must be read and written only there; a caller gets a verdict (VerifyLocalPassword) and an opaque PasswordCheck", owner, offenders)
	}

	// The owner itself must still name it, or the guard above guards nothing.
	if !namesTableInSQL(t, filepath.Join(root, owner), "operator_passwords") {
		t.Fatalf("%s no longer names operator_passwords: move this guard with the table", owner)
	}
}
