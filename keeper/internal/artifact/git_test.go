package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v5"
)

// TestOpenOrClone_HealsBrokenWorkClone reproduces keeper killed mid-clone: the
// work clone exists and PlainOpen succeeds, but there is no origin remote —
// without self-heal, a subsequent fetch would fail with ErrRemoteNotFound
// forever. We check that openOrClone detects the broken clone, re-clones with
// origin, and that fetch then succeeds.
func TestOpenOrClone_HealsBrokenWorkClone(t *testing.T) {
	tr := newTestRepo(t)
	workDir := filepath.Join(t.TempDir(), "_work")

	// Broken clone: initialize a repository without an origin remote.
	if _, err := git.PlainInit(workDir, false); err != nil {
		t.Fatalf("PlainInit битого клона: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err != nil {
		t.Fatalf("предусловие: .git не создан: %v", err)
	}

	repo, err := openOrClone(context.Background(), workDir, tr.fileURL(), nil)
	if err != nil {
		t.Fatalf("openOrClone: %v", err)
	}
	if _, rerr := repo.Remote("origin"); rerr != nil {
		t.Fatalf("origin-remote не восстановлен после self-heal: %v", rerr)
	}
	if err := fetch(context.Background(), repo, nil); err != nil {
		t.Fatalf("fetch после self-heal: %v", err)
	}
}

// TestOpenOrClone_KeepsHealthyClone marks the narrowness of the catch: we
// tear down ONLY a clone without origin. A healthy clone with origin must not
// be touched — we check that a repeated openOrClone reuses the same .git
// (modtime is preserved) instead of tearing it down.
func TestOpenOrClone_KeepsHealthyClone(t *testing.T) {
	tr := newTestRepo(t)
	workDir := filepath.Join(t.TempDir(), "_work")

	first, err := openOrClone(context.Background(), workDir, tr.fileURL(), nil)
	if err != nil {
		t.Fatalf("openOrClone #1: %v", err)
	}
	if _, rerr := first.Remote("origin"); rerr != nil {
		t.Fatalf("предусловие: первый клон без origin: %v", rerr)
	}
	gitInfo1, err := os.Stat(filepath.Join(workDir, ".git"))
	if err != nil {
		t.Fatalf("stat .git #1: %v", err)
	}

	if _, err := openOrClone(context.Background(), workDir, tr.fileURL(), nil); err != nil {
		t.Fatalf("openOrClone #2: %v", err)
	}
	gitInfo2, err := os.Stat(filepath.Join(workDir, ".git"))
	if err != nil {
		t.Fatalf("stat .git #2: %v", err)
	}
	if !gitInfo1.ModTime().Equal(gitInfo2.ModTime()) {
		t.Fatalf("здоровый клон пересоздан: .git modtime изменился (%v != %v)", gitInfo1.ModTime(), gitInfo2.ModTime())
	}
}
