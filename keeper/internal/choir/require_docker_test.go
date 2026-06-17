package choir

import "os"

// requireDocker — true, если CI требует обязательного docker-а (parity
// incarnation / operator / voyage).
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
