//go:build !unix

package config

import "os"

// statOwner-fallback для не-unix-платформ: ownership-перенос недоступен.
// Soul Stack целевая платформа — Linux/macOS (см. ADR-011 раскладка кода);
// поведение на Windows не нормируется, но компилируется.
func statOwner(_ os.FileInfo) (uid, gid int, ok bool) {
	return 0, 0, false
}
