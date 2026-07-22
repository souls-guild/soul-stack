package scenario

import "os"

// requireDocker reports whether CI requires docker.
// Same pattern as the applyrun / incarnation / topology packages.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
