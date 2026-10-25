package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxPasswordStdin bounds what `keeper init --password-stdin` reads: a password
// is at most 128 characters, so anything near this is not one.
const maxPasswordStdin = 4096

// readPasswordStdin reads the first Archon's password the way `docker login
// --password-stdin` does: the whole input, minus one trailing line ending — so
// both `printf '%s' "$PW" |` and `echo "$PW" |` give the same password.
func readPasswordStdin(r io.Reader) (string, error) {
	buf, err := io.ReadAll(io.LimitReader(r, maxPasswordStdin+1))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	if len(buf) > maxPasswordStdin {
		return "", fmt.Errorf("stdin holds more than %d bytes", maxPasswordStdin)
	}
	pw := strings.TrimSuffix(strings.TrimSuffix(string(buf), "\n"), "\r")
	if pw == "" {
		return "", errors.New("stdin is empty")
	}
	return pw, nil
}
