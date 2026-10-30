// The integration lane runs TestMain with containers, and the child process
// these guards start would bring up a second set.

//go:build !integration

package mcp

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/wiretime/wiretimetest"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

// TestMCPAnswers_AreUTCUnderANonUTCProcessZone is the NIM-1160 guard for MCP,
// end to end: a tool whose store hands back a time in the process zone, as pgx
// does, answers it in UTC in structuredContent and in the text block both.
func TestMCPAnswers_AreUTCUnderANonUTCProcessZone(t *testing.T) {
	if !wiretimetest.InSaoPaulo(t) {
		return
	}
	at := time.Unix(1793383200, 0) // 2026-10-30T14:00:00-03:00
	h, _ := newSigilKeyHandler(t, sigilKeyAdminCfg(), true)
	svc, err := sigil.NewKeyService(sigil.KeyServiceDeps{
		Pool:  &skPool{tx: &skTx{insertID: 1, insertTime: at}},
		Vault: skVault{},
	})
	if err != nil {
		t.Fatalf("sigil.NewKeyService: %v", err)
	}
	h.deps.SigilKeySvc = svc

	resp := callTool(t, h, "archon-alice", "keeper.sigil.key.introduce", `{"make_primary":true}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(res.Content))
	}
	for where, doc := range map[string][]byte{
		"structuredContent": res.StructuredContent,
		"content[0].text":   []byte(res.Content[0].Text),
	} {
		got := wiretimetest.Instants(t, doc)
		if len(got) != 1 {
			t.Fatalf("%s carries %d instants, want 1 (introduced_at):\n%s", where, len(got), doc)
		}
		for path, s := range got {
			p, _ := time.Parse(time.RFC3339Nano, s)
			if !strings.HasSuffix(s, "Z") || !p.Equal(at) {
				t.Errorf("%s %s = %s, want %s", where, path, s, at.UTC().Format(time.RFC3339))
			}
		}
	}
}

// TestMCPAnswers_LeaveOnlyThroughToolResult is why the end-to-end check above
// covers every tool. MCP output schemas are hand-written JSON with no Go type
// to fill, so the derivation is by exit instead: a tool answer is built only by
// toolResult, which moves every instant to UTC (the encoder guard in
// keeper/internal/api holds that), and a JSON-RPC result only by the three
// methods below.
func TestMCPAnswers_LeaveOnlyThroughToolResult(t *testing.T) {
	results := map[string]bool{"handleInitialize": true, "handleToolsList": true, "toolResult": true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "newRPCResult" {
						seen[fn.Name.Name] = true
						if !results[fn.Name.Name] {
							t.Errorf("%s: %s builds a JSON-RPC result itself; answer through toolResult", fset.Position(x.Pos()), fn.Name.Name)
						}
					}
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "toolsCallResult" && fn.Name.Name != "toolResult" {
						t.Errorf("%s: %s builds a tool answer itself; answer through toolResult", fset.Position(x.Pos()), fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if !seen["toolResult"] {
		t.Fatal("toolResult no longer calls newRPCResult; the scan is not reading the tool exit")
	}
}
