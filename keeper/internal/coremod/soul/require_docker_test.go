package soul_test

import "os"

// requireDocker reports whether CI requires Docker
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Mirrors the behavior in
// keeper/internal/soul/require_docker_test.go.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
