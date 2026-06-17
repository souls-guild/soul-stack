package augur

import "os"

// requireDocker — true, если CI требует обязательного docker-а.
// Паттерн совпадает с provider / operator / incarnation пакетами.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
