package artifact

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// reCacheName restricts the service name to a safe character set for the
// cache path's file segment. Matches the canonical kebab-case from
// `shared/config` (reServiceName), but is checked here independently: the
// name comes from ServiceRef before `service.yml` is even parsed, so we
// can't rely on manifest validation. Guards against `..`/`/` in the first
// segment of the cache path.
var reCacheName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// cacheLayout encapsulates one service's cache layout under cacheRoot:
//
//	<cacheRoot>/<name>/_work/        — working clone (fetch+checkout target)
//	<cacheRoot>/<name>/<sha1>/       — immutable tree snapshot at a commit
//	<cacheRoot>/<name>/_tmp-*/       — staging for atomic snapshot rename
type cacheLayout struct {
	root string
	name string
}

func newCacheLayout(root, name string) (cacheLayout, error) {
	if !reCacheName.MatchString(name) {
		return cacheLayout{}, fmt.Errorf("artifact: имя сервиса %q не является kebab-case (защита cache-пути)", name)
	}
	return cacheLayout{root: root, name: name}, nil
}

// serviceDir — the service's directory (`<cacheRoot>/<name>`).
func (c cacheLayout) serviceDir() string { return filepath.Join(c.root, c.name) }

// workDir — the service's working clone.
func (c cacheLayout) workDir() string { return filepath.Join(c.serviceDir(), "_work") }

// snapshotDir — immutable tree snapshot at a specific sha1.
func (c cacheLayout) snapshotDir(sha1 string) string {
	return filepath.Join(c.serviceDir(), sha1)
}

// ensureServiceDir creates the service directory (recursively) if missing.
func (c cacheLayout) ensureServiceDir() error {
	if err := os.MkdirAll(c.serviceDir(), 0o755); err != nil {
		return fmt.Errorf("artifact: создание cache-каталога %s: %w", c.serviceDir(), err)
	}
	return nil
}

// snapshotExists reports whether the snapshot for sha1 is materialized.
func (c cacheLayout) snapshotExists(sha1 string) bool {
	info, err := os.Stat(c.snapshotDir(sha1))
	return err == nil && info.IsDir()
}

// newStagingDir creates an empty temp directory under serviceDir for
// snapshot staging. Lives on the same filesystem as snapshotDir — a
// requirement for atomic os.Rename.
func (c cacheLayout) newStagingDir() (string, error) {
	dir, err := os.MkdirTemp(c.serviceDir(), "_tmp-")
	if err != nil {
		return "", fmt.Errorf("artifact: создание staging-каталога: %w", err)
	}
	return dir, nil
}
