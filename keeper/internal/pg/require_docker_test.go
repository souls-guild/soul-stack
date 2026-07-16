package pg

import "os"

// requireDocker returns true if CI requires mandatory Docker
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). The pattern matches
// keeper/internal/auditpg/require_docker_test.go.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
