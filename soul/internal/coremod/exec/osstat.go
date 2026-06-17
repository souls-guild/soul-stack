package exec

import (
	"errors"
	"io/fs"
	"os"
)

// osStat — production-обёртка над os.Stat для StatFile. Возвращает (true, nil)
// для существующего пути, (false, nil) для отсутствующего, (false, err) — для
// прочих ошибок (permission denied и пр.).
func osStat(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}
