package provider

import "os"

// requireDocker is true when CI requires Docker.
// Pattern matches operator / incarnation / api packages.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
