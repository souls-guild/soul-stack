package vault

import "os"

// requireDocker is true if CI requires docker to be mandatory
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). The pattern matches
// keeper/internal/auditpg/require_docker_test.go; kept in a tag-less file so
// integration_test.go under `//go:build integration` can see it, while the
// separate unit tests for this env parsing stay independent.
//
// The regression test for env parsing lives in
// auditpg/require_docker_test.go; no need to duplicate it here — the
// semantics are shared.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
