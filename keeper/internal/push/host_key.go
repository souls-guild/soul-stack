package push

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/crypto/ssh"
)

// HostKeyError is a host whose key could not be verified: the key it presented
// was refused, or there was nothing to verify it against.
//
// It is a type rather than a message because a connect-retry loop has to tell it
// apart from "the host is not up yet" without reading text: waiting cannot make a
// refused key acceptable, and spending a join budget on one turns a clear refusal
// into a timeout (NIM-905). Both SSH stacks in use keep a callback's error
// reachable by [errors.As] — x/crypto wraps it with %w, Teleport's `trace` aggregate
// implements As.
type HostKeyError struct {
	Err error
}

func (e *HostKeyError) Error() string { return "host key not verified: " + e.Err.Error() }

func (e *HostKeyError) Unwrap() error { return e.Err }

// IsHostKeyError reports whether err is, or wraps, a [HostKeyError].
func IsHostKeyError(err error) bool {
	var hk *HostKeyError
	return errors.As(err, &hk)
}

// errNoHostKeyCallback is what a Teleport identity file without an SSH CA
// produces: no `known_hosts`, so no callback, so nothing to verify a host against.
var errNoHostKeyCallback = errors.New("the identity carries no SSH CA (known_hosts), so no host can be verified")

// tagHostKeyCallback makes every refusal cb returns a [HostKeyError].
//
// ★ A nil cb stays nil. A wrapper that called through nil would panic inside the
// key exchange, and one that read nil as "accept" would be a blind connect that no
// search for `InsecureIgnoreHostKey` finds. The SSH library refuses a nil callback
// itself; the callers here refuse it first, as a HostKeyError.
func tagHostKeyCallback(cb ssh.HostKeyCallback) ssh.HostKeyCallback {
	if cb == nil {
		return nil
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := cb(hostname, remote, key); err != nil {
			return &HostKeyError{Err: fmt.Errorf("%s: %w", hostname, err)}
		}
		return nil
	}
}
