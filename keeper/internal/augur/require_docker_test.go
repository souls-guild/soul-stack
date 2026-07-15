package augur

import "os"

// requireDocker — true if CI requires docker to be mandatory.
// Matches the pattern used by the provider / operator / incarnation packages.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
