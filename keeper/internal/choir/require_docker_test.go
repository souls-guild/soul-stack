package choir

import "os"

// requireDocker reports whether CI requires docker to be mandatory (parity
// incarnation / operator / voyage).
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
