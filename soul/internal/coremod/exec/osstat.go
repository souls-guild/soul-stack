package exec

import (
	"errors"
	"io/fs"
	"os"
)

// osStat is the production wrapper over os.Stat for StatFile. Returns
// (true, nil) if the path exists, (false, nil) if it doesn't, and
// (false, err) for other errors (permission denied etc).
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
