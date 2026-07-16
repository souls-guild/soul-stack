//go:build integration

package voyage

import "os"

// requireDocker — true if CI requires mandatory docker (parity
// choir / incarnation / operator).
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
