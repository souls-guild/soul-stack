package artifact

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrUnsupportedGitScheme — the git URL scheme is not allowed for loading an
// artifact (security review L2). By default only `https://`, `ssh://`, and
// the scp form `user@host:path` are in the allowlist; `file://` requires an
// explicit env flag.
var ErrUnsupportedGitScheme = errors.New("artifact: unsupported git-URL scheme")

// allowFileReposEnv — env flag allowing `file://` repositories (dev/test).
// Off by default in prod: `file://` would let ServiceRef.Git read local
// repositories on the keeper host (security review L2).
const allowFileReposEnv = "SOUL_STACK_ALLOW_FILE_REPOS"

// validateGitScheme checks that gitURL's scheme is in the prod allowlist
// (`https://`, `ssh://`, scp form). `file://` passes only when
// SOUL_STACK_ALLOW_FILE_REPOS=1. Any other scheme (including `http://`) —
// ErrUnsupportedGitScheme.
func validateGitScheme(gitURL string) error {
	switch {
	case strings.HasPrefix(gitURL, "https://"):
		return nil
	case strings.HasPrefix(gitURL, "ssh://"):
		return nil
	case strings.HasPrefix(gitURL, "file://"):
		if fileReposAllowed() {
			return nil
		}
		return fmt.Errorf("%w: file:// is forbidden in production (set %s=1 for dev/test): %q",
			ErrUnsupportedGitScheme, allowFileReposEnv, gitURL)
	case !strings.Contains(gitURL, "://") && isSCPForm(gitURL):
		// scp-like form `git@host:org/repo.git` (SSH without an explicit scheme).
		return nil
	default:
		return fmt.Errorf("%w: %q (allowed: https://, ssh://, scp form user@host:path)",
			ErrUnsupportedGitScheme, gitURL)
	}
}

// isSCPForm recognizes the scp-like form `user@host:path` (a colon after `@`,
// no scheme) — the same rule isSSHURL in git.go uses.
func isSCPForm(gitURL string) bool {
	at := strings.Index(gitURL, "@")
	colon := strings.Index(gitURL, ":")
	return at >= 0 && colon > at
}

// fileReposAllowed reports whether `file://` repositories are allowed via the env flag.
func fileReposAllowed() bool {
	return os.Getenv(allowFileReposEnv) == "1"
}
