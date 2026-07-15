package incarnation

import "os"

// requireDocker — true if CI requires docker to be mandatory.
// The pattern matches the operator / auditpg / api packages.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
