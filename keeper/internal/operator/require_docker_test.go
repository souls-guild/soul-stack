package operator

import "os"

// requireDocker — true if CI requires docker
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). See description in
// keeper/internal/auditpg/require_docker_test.go — behavior is identical,
// keep helper alongside package integration tests.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
