package console

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The frame plane has exactly one home, and it is not this package.
//
// Queue depth, socket deadlines and the inbound frame cap describe the WebSocket
// implementation in internal/api, which owns them. This package once carried a
// second copy of all five, referenced by nothing: Go does not complain about an
// unused constant, so editing the dead copy changed nothing, while editing the
// live one drifted away from a comment claiming the two were mirrors. Both
// failures are silent, which is why this guards the split rather than the values
// — a re-added copy is the defect whatever it is set to.
func TestFramePlaneConstantsHaveNoSecondHomeHere(t *testing.T) {
	owned := map[string]string{
		"outQueueDepth":      "consoleOutQueueDepth",
		"writeWait":          "consoleWriteWait",
		"pongWait":           "consolePongWait",
		"pingPeriod":         "consolePingPeriod",
		"maxClientFrameSize": "consoleMaxClientFrame",
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						if live, dup := owned[name.Name]; dup {
							t.Errorf("%s declares %q — the frame plane belongs to internal/api (%s). A copy here is referenced by nothing, so it drifts in silence: Go does not flag an unused constant.",
								path, name.Name, live)
						}
					}
				}
			}
		}
	}
}
