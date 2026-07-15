package cloud_test

import "os"

// requireDocker reports whether CI requires Docker
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Mirrors the neighboring
// integration_test.go in keeper/internal/coremod/{soul,vault}.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
