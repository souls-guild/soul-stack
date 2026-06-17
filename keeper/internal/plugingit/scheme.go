package plugingit

import (
	"fmt"
	"os"
	"strings"
)

// allowFileReposEnv — env-флаг, разрешающий `file://`-источники плагинов
// (dev/test). В проде по умолчанию выключен: `file://` дал бы git-резолву
// читать локальные репозитории keeper-host-а через config-значение `source`
// (та же модель, что в internal/artifact, security review L2).
const allowFileReposEnv = "SOUL_STACK_ALLOW_FILE_REPOS"

// validateGitScheme проверяет, что схема source входит в prod-allowlist
// (`https://`, `ssh://`, scp-форма `user@host:path`). `file://` пропускается
// только при SOUL_STACK_ALLOW_FILE_REPOS=1. Любая иная схема (включая
// незашифрованный `http://`) отвергается — это первый рубеж против опасных
// транспортов: go-git не знает `ext::`, а `file://` мы запираем здесь, а не
// флагом библиотеки.
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
		return fmt.Errorf("%w: file:// запрещён в проде (выставьте %s=1 для dev/test): %q",
			ErrSourceUnavailable, allowFileReposEnv, source)
	case !strings.Contains(source, "://") && isSSHURL(source):
		// scp-подобная форма `git@host:org/repo.git` (SSH без явной схемы).
		return nil
	default:
		return fmt.Errorf("%w: неподдерживаемая схема %q (разрешены https://, ssh://, scp-форма user@host:path)",
			ErrSourceUnavailable, source)
	}
}
