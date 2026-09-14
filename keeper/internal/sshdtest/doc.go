// Package sshdtest brings up a real sshd in a container, with a host
// certificate and a trusted user CA, for tests of the push path.
//
// It exists because push has two suites that need the same thing and no way to
// share it otherwise: a dispatcher-level one in `internal/push` and an
// orchestrator-level one in `internal/pushorch`, and a Go test helper cannot
// cross a package boundary. Duplicating it is how the two drift until only one
// of them is a real host.
//
// Everything here is behind `//go:build integration`; under the default build
// this package is empty on purpose.
package sshdtest
