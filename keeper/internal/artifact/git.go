package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// gitRepo — an alias for an open go-git repository. Keeps the go-git import
// confined to git.go: the rest of the package works with the repository as an
// opaque handle.
type gitRepo = git.Repository

// fetchAllRefSpec updates all branches and tags in the working clone in force
// mode. Without it, a branch that advanced on the remote would not be visible
// to the local ResolveRevision (PM decision: always fetch + checkout).
const fetchAllRefSpec config.RefSpec = "+refs/heads/*:refs/remotes/origin/*"

// openOrClone opens the working clone under workDir, or clones it if the
// directory is not yet a repository, or the existing clone is broken (no
// origin remote — keeper was killed mid-clone). Returns the open repository.
func openOrClone(ctx context.Context, workDir, gitURL string, auth transport.AuthMethod) (*gitRepo, error) {
	repo, err := git.PlainOpen(workDir)
	if err == nil {
		// Self-heal a broken clone: keeper killed mid-clone leaves a directory
		// that PlainOpen opens fine, but without an origin remote — a
		// subsequent fetch fails with ErrRemoteNotFound FOREVER until a manual
		// cleanup. We catch strictly ErrRemoteNotFound (transient config-read
		// errors are not treated as broken) and re-clone. Access to workDir is
		// serialized by a per-name mutex in snapshotter.snapshot, so there is
		// no race with this RemoveAll.
		_, rerr := repo.Remote("origin")
		if rerr == nil {
			return repo, nil
		}
		if !errors.Is(rerr, git.ErrRemoteNotFound) {
			return nil, fmt.Errorf("artifact: checking origin remote of clone %s: %w", workDir, rerr)
		}
		// Fall through to the common clone branch below via RemoveAll +
		// PlainCloneContext.
	} else if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("artifact: opening working clone %s: %w", workDir, err)
	}

	// The directory might have existed as empty/broken — remove it before a
	// clean clone, so PlainCloneContext doesn't fail with
	// ErrRepositoryAlreadyExists.
	if rmErr := os.RemoveAll(workDir); rmErr != nil {
		return nil, fmt.Errorf("artifact: cleaning before clone %s: %w", workDir, rmErr)
	}
	repo, err = git.PlainCloneContext(ctx, workDir, false, &git.CloneOptions{
		URL:  gitURL,
		Auth: auth,
		Tags: git.AllTags,
	})
	if err != nil {
		return nil, fmt.Errorf("artifact: cloning %s: %w", gitURL, err)
	}
	return repo, nil
}

// fetch updates the working clone from the remote. NoErrAlreadyUpToDate is
// not an error.
func fetch(ctx context.Context, repo *gitRepo, auth transport.AuthMethod) error {
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{fetchAllRefSpec},
		Tags:     git.AllTags,
		Force:    true,
		Auth:     auth,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("artifact: fetch: %w", err)
	}
	return nil
}

// resolveRef resolves a git ref (tag/branch/HEAD) to a commit hash. An empty
// ref means HEAD.
//
// The remote-tracking form (`refs/remotes/origin/<ref>`) is checked BEFORE
// the bare `<ref>`: fetch places branches under `refs/remotes/origin/*` (see
// fetchAllRefSpec) and advances them, whereas the local `refs/heads/<ref>`
// from the initial clone stays on the old commit (we do a fetch, not a pull).
// Without this priority, an advanced branch would resolve to a stale tip.
//
// Order: tag → remote-tracking branch → as-is (full hash / HEAD).
func resolveRef(repo *gitRepo, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	candidates := []plumbing.Revision{
		plumbing.Revision("refs/tags/" + ref),
		plumbing.Revision("refs/remotes/origin/" + ref),
		plumbing.Revision(ref),
	}
	var lastErr error
	for _, rev := range candidates {
		hash, err := repo.ResolveRevision(rev)
		if err == nil {
			return hash.String(), nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("artifact: ref %q does not resolve in repository: %w", ref, lastErr)
}

// checkout puts the working clone into a detached-HEAD state at the given
// commit. Force — to overwrite leftovers from a previous checkout without
// manually cleaning the worktree.
func checkout(repo *gitRepo, sha1 string) error {
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("artifact: worktree: %w", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{
		Hash:  plumbing.NewHash(sha1),
		Force: true,
	}); err != nil {
		return fmt.Errorf("artifact: checkout %s: %w", sha1, err)
	}
	return nil
}

// exportTree copies the working clone's file tree from src to dst, skipping
// the `.git` directory. The snapshot remains a clean service tree without
// git metadata.
func exportTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Skip `.git` and everything under it entirely.
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			// Symlinks and special files are not carried into the snapshot —
			// the render phase reads only regular files, a symlink target is
			// a potential escape.
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// authFor picks the auth method by the URL scheme. SSH (`ssh://` or the scp
// form `user@host:path`) — via SSH agent (PM decision; Vault auth is
// post-MVP). `file://`/`https://` need no auth (MVP).
func authFor(gitURL string) (transport.AuthMethod, error) {
	if !isSSHURL(gitURL) {
		return nil, nil
	}
	user := sshUser(gitURL)
	auth, err := gitssh.NewSSHAgentAuth(user)
	if err != nil {
		return nil, fmt.Errorf("artifact: SSH-agent auth for %s: %w", gitURL, err)
	}
	return auth, nil
}

// isSSHURL recognizes the ssh scheme and the scp-like form
// `git@host:org/repo.git`.
func isSSHURL(gitURL string) bool {
	if strings.HasPrefix(gitURL, "ssh://") {
		return true
	}
	if strings.Contains(gitURL, "://") {
		return false
	}
	// scp form: `user@host:path`, a colon after the host, no scheme.
	at := strings.Index(gitURL, "@")
	colon := strings.Index(gitURL, ":")
	return at >= 0 && colon > at
}

// sshUser extracts the username from an ssh URL; defaults to `git`.
func sshUser(gitURL string) string {
	if strings.HasPrefix(gitURL, "ssh://") {
		if u, err := url.Parse(gitURL); err == nil && u.User != nil {
			if name := u.User.Username(); name != "" {
				return name
			}
		}
		return "git"
	}
	if at := strings.Index(gitURL, "@"); at > 0 {
		return gitURL[:at]
	}
	return "git"
}
