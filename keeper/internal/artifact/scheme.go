package artifact

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrUnsupportedGitScheme — схема git-URL не разрешена для загрузки артефакта
// (security review L2). По умолчанию в allowlist только `https://`, `ssh://` и
// scp-форма `user@host:path`; `file://` требует явного env-флага.
var ErrUnsupportedGitScheme = errors.New("artifact: неподдерживаемая схема git-URL")

// allowFileReposEnv — env-флаг, разрешающий `file://`-репозитории (dev/test).
// В проде по умолчанию выключен: `file://` позволяет читать локальные репозитории
// keeper-хоста через ServiceRef.Git (security review L2).
const allowFileReposEnv = "SOUL_STACK_ALLOW_FILE_REPOS"

// validateGitScheme проверяет, что схема gitURL входит в prod-allowlist
// (`https://`, `ssh://`, scp-форма). `file://` пропускается только при
// SOUL_STACK_ALLOW_FILE_REPOS=1. Любая иная схema (включая `http://`) —
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
		return fmt.Errorf("%w: file:// запрещён в проде (выставьте %s=1 для dev/test): %q",
			ErrUnsupportedGitScheme, allowFileReposEnv, gitURL)
	case !strings.Contains(gitURL, "://") && isSCPForm(gitURL):
		// scp-подобная форма `git@host:org/repo.git` (SSH без явной схемы).
		return nil
	default:
		return fmt.Errorf("%w: %q (разрешены https://, ssh://, scp-форма user@host:path)",
			ErrUnsupportedGitScheme, gitURL)
	}
}

// isSCPForm распознаёт scp-подобную форму `user@host:path` (двоеточие после `@`,
// без схемы) — то же правило, что у isSSHURL в git.go.
func isSCPForm(gitURL string) bool {
	at := strings.Index(gitURL, "@")
	colon := strings.Index(gitURL, ":")
	return at >= 0 && colon > at
}

// fileReposAllowed сообщает, разрешены ли `file://`-репозитории через env-флаг.
func fileReposAllowed() bool {
	return os.Getenv(allowFileReposEnv) == "1"
}
