//go:build integration

package trial

import "os"

// requireDocker — true if CI requires mandatory docker
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Pattern matches other
// integration tests of keeper (topology/applyrun/...): without flag and without docker —
// test skip; with flag — fatal if docker unavailable.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
