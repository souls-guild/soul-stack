package plugingit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// Git layer of resolver — pure-Go via go-git. System `git` binary is NOT forked:
// keeper-host does not require `git` runtime dependency, and git-egress
// (HIGH security risk, ADR-026) is protected by-design by the library itself, no flags:
//
//   - hooks: go-git does NOT execute git-hooks on clone or checkout —
//     adversarial repo cannot invoke post-checkout/pre-commit from service-user.
//     This is go-git behavior by design, not a configurable flag.
//   - protocol ext::: transport `ext::<cmd>` (executing external command as
//     transport) does not exist in go-git at all — attack vector absent.
//   - protocol file://: controlled NOT by go-git, but by input URL scheme validation
//     (see [validateGitScheme]) — allowlist https/ssh, file:// only under
//     env flag for dev/test.
//   - submodules: NOT passing RecurseSubmodules — go-git default = no recursion;
//     nested submodule sources not cloned.
//   - depth: clone/fetch use Depth=1 (shallow). If remote degrades to
//     full clone — this is acceptable (not an error), resolve still correct.
//   - timeout: clone/fetch executed via *Context variants with ctx from
//     fetch_timeout — external call always time-bounded.

// shallowDepth — depth of shallow clone/fetch for resolver (F-fetch: need exactly
// one ref commit, not history).
const shallowDepth = 1

// fetchAllRefSpec updates all branches in working clone in force mode to
// remote-tracking. Narrow refspec on specific ref in go-git — hard error if
// ref is a tag (heads/<ref> does not exist); wildcard + Tags=AllTags below
// covers both branches and tags in one operation (pattern artifact/git.go).
const fetchAllRefSpec config.RefSpec = "+refs/heads/*:refs/remotes/origin/*"

// openOrClone opens working clone at workDir or clones it shallow if directory
// is not yet a repository. Broken/empty directory is recreated with clean clone.
// ctx bounds clone by time (fetch_timeout).
func openOrClone(ctx context.Context, workDir, gitURL string, auth transport.AuthMethod) (*git.Repository, error) {
	repo, err := git.PlainOpen(workDir)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("plugingit: open working clone %s: %w", workDir, err)
	}

	// Directory may exist as empty/broken — remove before clean clone
	// to avoid PlainCloneContext error on ErrRepositoryAlreadyExists.
	if rmErr := os.RemoveAll(workDir); rmErr != nil {
		return nil, fmt.Errorf("plugingit: cleanup before clone %s: %w", workDir, rmErr)
	}
	repo, err = git.PlainCloneContext(ctx, workDir, false, &git.CloneOptions{
		URL:   gitURL,
		Auth:  auth,
		Depth: shallowDepth,
		Tags:  git.AllTags,
	})
	if err != nil {
		return nil, fmt.Errorf("plugingit: clone %s: %w", gitURL, err)
	}
	return repo, nil
}

// fetch updates working clone with remote shallow fetch: all branches in
// remote-tracking + all tags (Tags=AllTags). Covers both branch and tag in one
// operation so [resolveRef] finds target regardless of ref type.
// NoErrAlreadyUpToDate — not an error.
func fetch(ctx context.Context, repo *git.Repository, auth transport.AuthMethod) error {
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{fetchAllRefSpec},
		Depth:    shallowDepth,
		Force:    true,
		Tags:     git.AllTags,
		Auth:     auth,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("plugingit: fetch: %w", err)
	}
	return nil
}

// resolveRef resolves git-ref (tag/branch/commit) to full 40-hex commit-hash
// via ResolveRevision with suffix `^{commit}` (peel: tag object → commit,
// guarantee ref points to commit). Type plumbing.Hash guarantees 40-hex output.
//
// Candidate order: tag → remote-tracking branch → as-is (full hash).
// Remote-tracking form checked BEFORE bare `<ref>`: shallow fetch places
// branch in `refs/remotes/origin/<ref>` and advances it, while local
// `refs/heads/<ref>` from original clone stays on old commit.
func resolveRef(repo *git.Repository, ref string) (string, error) {
	candidates := []plumbing.Revision{
		plumbing.Revision("refs/tags/" + ref + "^{commit}"),
		plumbing.Revision("refs/remotes/origin/" + ref + "^{commit}"),
		plumbing.Revision(ref + "^{commit}"),
	}
	for _, rev := range candidates {
		hash, err := repo.ResolveRevision(rev)
		if err == nil {
			return hash.String(), nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrRefNotResolved, ref)
}

// checkout moves working clone to detached-HEAD at specified commit. Force —
// to overwrite remnants of previous checkout without manual worktree cleanup.
// go-git does NOT execute hooks on checkout (by design).
func checkout(repo *git.Repository, sha1 string) error {
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("plugingit: worktree: %w", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{
		Hash:  plumbing.NewHash(sha1),
		Force: true,
	}); err != nil {
		return fmt.Errorf("plugingit: checkout %s: %w", sha1, err)
	}
	return nil
}

// authFor selects auth method by URL scheme. SSH (`ssh://` or scp form
// `user@host:path`) — via SSH-agent (PM-decision, symmetric with artifact/git.go;
// Vault-auth post-MVP). `file://`/`https://` auth not required (MVP).
func authFor(gitURL string) (transport.AuthMethod, error) {
	if !isSSHURL(gitURL) {
		return nil, nil
	}
	auth, err := gitssh.NewSSHAgentAuth(sshUser(gitURL))
	if err != nil {
		return nil, fmt.Errorf("plugingit: SSH-agent auth for %s: %w", gitURL, err)
	}
	return auth, nil
}

// isSSHURL recognizes ssh scheme and scp-like form `git@host:org/repo.git`.
func isSSHURL(gitURL string) bool {
	if strings.HasPrefix(gitURL, "ssh://") {
		return true
	}
	if strings.Contains(gitURL, "://") {
		return false
	}
	// scp form: `user@host:path`, colon after host, no scheme.
	at := strings.Index(gitURL, "@")
	colon := strings.Index(gitURL, ":")
	return at >= 0 && colon > at
}

// sshUser extracts username from ssh-URL; defaults to `git`.
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
