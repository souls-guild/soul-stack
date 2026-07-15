package rbac

import "os"

// requireDocker reports whether CI requires Docker to be available
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Mirrors the helper in
// sibling packages (operator/migrate); kept next to this package's
// integration tests.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
