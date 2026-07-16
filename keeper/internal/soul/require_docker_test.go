package soul

import "os"

// requireDocker reports whether CI requires docker
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Mirrors
// keeper/internal/operator/require_docker_test.go.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
