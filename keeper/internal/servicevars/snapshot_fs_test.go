package servicevars

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// TestLexicalView_SymlinkOutOfTreeIsNotRead — the containment guard on the view
// with no incarnation in hand, which feeds `GET /v1/services/{id}/directives`. A
// symlink out of the snapshot, as a layer file or as `vars/` itself, is re-rooted
// under the snapshot by securejoin and the file behind it is not read: the
// lexical scan lists the link and skips it as missing.
func TestLexicalView_SymlinkOutOfTreeIsNotRead(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.yaml")
	if err := os.WriteFile(secret, []byte("leaked: true\n"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "00-base.yaml"), []byte("leaked: true\n"), 0o644); err != nil {
		t.Fatalf("write outside base: %v", err)
	}
	symlink := func(t *testing.T, target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}

	t.Run("layer file", func(t *testing.T) {
		dir := writeLayers(t, map[string]string{"00-base.yaml": "key: base\n"})
		symlink(t, secret, filepath.Join(dir, "vars", "10-link.yaml"))
		got := materializeLexical(t, dir)
		if _, leaked := got["leaked"]; leaked {
			t.Fatal("securejoin did not clamp the symlink: a file outside the snapshot was read")
		}
		if got["key"] != "base" {
			t.Fatalf("got=%#v", got)
		}
	})

	t.Run("vars directory", func(t *testing.T) {
		dir := t.TempDir()
		symlink(t, outside, filepath.Join(dir, "vars"))
		if got := materializeLexical(t, dir); len(got) != 0 {
			t.Fatalf("securejoin did not clamp the symlinked vars/: got %#v", got)
		}
	})
}

func materializeLexical(t *testing.T, dir string) map[string]any {
	t.Helper()
	view, err := NewResolver(nil).LexicalView(dir)
	if err != nil {
		t.Fatalf("LexicalView: %v", err)
	}
	got, err := view.Materialize()
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return got
}

// TestViews_InvalidLayerNameIsRefusedByName — a layer file whose name is not valid
// UTF-8 fails fs.ValidPath, on a real filesystem as on the test FS of
// shared/servicevars. Both views refuse it naming the file and the reason, where
// they used to answer the snapshot FS's bare "invalid argument".
func TestViews_InvalidLayerNameIsRefusedByName(t *testing.T) {
	dir := writeLayers(t, map[string]string{"00-base.yaml": "a: 1\n"})
	if err := os.WriteFile(filepath.Join(dir, "vars", "\xff-bad.yaml"), []byte("b: 2\n"), 0o644); err != nil {
		t.Skipf("the filesystem refuses a non-UTF-8 name: %v", err)
	}
	r := NewResolver(nil)
	for name, build := range map[string]func() (*cel.VarView, error){
		"View":        func() (*cel.VarView, error) { return r.View(ResolveInput{ServiceDir: dir}) },
		"LexicalView": func() (*cel.VarView, error) { return r.LexicalView(dir) },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := build()
			if err == nil {
				t.Fatal("a layer file with an invalid name must be refused")
			}
			if !strings.Contains(err.Error(), `"vars/\xff-bad.yaml"`) || !strings.Contains(err.Error(), "not a valid path") {
				t.Fatalf("the error must name the file and say why: %v", err)
			}
		})
	}
}
