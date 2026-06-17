package artifact

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// reCacheName ограничивает имя сервиса безопасным набором символов для
// файлового сегмента кеша. Совпадает с canonical kebab-case из
// `shared/config` (reServiceName), но проверяется здесь самостоятельно: имя
// приходит из ServiceRef ещё до парсинга `service.yml`, поэтому полагаться на
// валидацию манифеста нельзя. Защита от `..`/`/` в первом сегменте cache-пути.
var reCacheName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// cacheLayout инкапсулирует раскладку кеша одного сервиса под cacheRoot:
//
//	<cacheRoot>/<name>/_work/        — рабочий клон (fetch+checkout сюда)
//	<cacheRoot>/<name>/<sha1>/       — immutable-снапшот дерева на commit-е
//	<cacheRoot>/<name>/_tmp-*/       — staging для атомарного rename снапшота
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

// serviceDir — каталог сервиса (`<cacheRoot>/<name>`).
func (c cacheLayout) serviceDir() string { return filepath.Join(c.root, c.name) }

// workDir — рабочий клон сервиса.
func (c cacheLayout) workDir() string { return filepath.Join(c.serviceDir(), "_work") }

// snapshotDir — immutable-снапшот дерева на конкретном sha1.
func (c cacheLayout) snapshotDir(sha1 string) string {
	return filepath.Join(c.serviceDir(), sha1)
}

// ensureServiceDir создаёт каталог сервиса (рекурсивно), если его нет.
func (c cacheLayout) ensureServiceDir() error {
	if err := os.MkdirAll(c.serviceDir(), 0o755); err != nil {
		return fmt.Errorf("artifact: создание cache-каталога %s: %w", c.serviceDir(), err)
	}
	return nil
}

// snapshotExists сообщает, материализован ли снапшот для sha1.
func (c cacheLayout) snapshotExists(sha1 string) bool {
	info, err := os.Stat(c.snapshotDir(sha1))
	return err == nil && info.IsDir()
}

// newStagingDir создаёт пустой временный каталог под serviceDir для staging-а
// снапшота. Лежит на той же файловой системе, что и snapshotDir, — это условие
// атомарного os.Rename.
func (c cacheLayout) newStagingDir() (string, error) {
	dir, err := os.MkdirTemp(c.serviceDir(), "_tmp-")
	if err != nil {
		return "", fmt.Errorf("artifact: создание staging-каталога: %w", err)
	}
	return dir, nil
}
