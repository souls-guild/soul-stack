package serviceregistry

import "os"

// requireDocker reports whether CI requires docker to be mandatory
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Pattern matches the
// augur / rbac / provider packages.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
