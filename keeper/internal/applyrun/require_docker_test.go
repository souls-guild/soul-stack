package applyrun

import "os"

// requireDocker reports whether CI requires docker to be mandatory.
// Mirrors the pattern used by the incarnation / operator / auditpg / api packages.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
