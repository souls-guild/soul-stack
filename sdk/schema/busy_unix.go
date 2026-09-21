//go:build unix

package schema

import (
	"errors"
	"syscall"
)

// isTextFileBusy reports ETXTBSY — "the kernel still holds this file as a running
// image". A build that stamps straight after `go build` hits it routinely: the Go
// toolchain's own fork/exec can hold a descriptor on a freshly written executable for a
// moment after the process that ran it has exited.
func isTextFileBusy(err error) bool { return errors.Is(err, syscall.ETXTBSY) }
