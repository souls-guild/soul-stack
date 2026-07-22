package plugingit

import (
	"fmt"
	"os"
	"strings"
)

// allowFileReposEnv — env flag permitting `file://` plugin sources
// (dev/test). Off by default in prod: `file://` would let git-resolve
// read local keeper-host repositories via config value `source`
// (same model as in internal/artifact, security review L2).
const allowFileReposEnv = "SOUL_STACK_ALLOW_FILE_REPOS"

// validateGitScheme checks that source scheme in prod-allowlist
// (`https://`, `ssh://`, scp form `user@host:path`). `file://` passed
// only on SOUL_STACK_ALLOW_FILE_REPOS=1. Any other scheme (including
// unencrypted `http://`) rejected — first line of defense against dangerous
// transports: go-git doesn't know `ext::`, and `file://` we lock here, not
// by library flag.
func validateGitScheme(source string) error {
	switch {
	case strings.HasPrefix(source, "https://"):
		return nil
	case strings.HasPrefix(source, "ssh://"):
		return nil
	case strings.HasPrefix(source, "file://"):
		if os.Getenv(allowFileReposEnv) == "1" {
			return nil
		}
		return fmt.Errorf("%w: file:// forbidden in prod (set %s=1 for dev/test): %q",
			ErrSourceUnavailable, allowFileReposEnv, source)
	case !strings.Contains(source, "://") && isSSHURL(source):
		// scp-like form `git@host:org/repo.git` (SSH without explicit scheme).
		return nil
	default:
		return fmt.Errorf("%w: unsupported scheme %q (allowed https://, ssh://, scp form user@host:path)",
			ErrSourceUnavailable, source)
	}
}
