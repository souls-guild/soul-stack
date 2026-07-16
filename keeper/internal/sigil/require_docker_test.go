package sigil

import "os"

// requireDocker returns true if CI requires mandatory docker
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Behavior identical to
// keeper/internal/operator/require_docker_test.go.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
