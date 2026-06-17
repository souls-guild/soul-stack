package applyrun

import "os"

// requireDocker — true, если CI требует обязательного docker-а.
// Паттерн совпадает с incarnation / operator / auditpg / api пакетами.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
