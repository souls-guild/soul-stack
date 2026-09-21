//go:build !unix

package config

import "os"

// statOwner fallback for non-unix platforms: ownership transfer is unavailable.
// Soul Stack targets Linux/macOS (see ADR-011 code layout); Windows behavior is
// unspecified but compiles.
func statOwner(_ os.FileInfo) (uid, gid int, ok bool) {
	return 0, 0, false
}
