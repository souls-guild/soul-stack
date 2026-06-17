package operator

import "os"

// requireDocker — true, если CI требует обязательного docker-а
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). См. описание в
// keeper/internal/auditpg/require_docker_test.go — поведение идентичное,
// держим helper рядом с integration-тестами пакета.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
