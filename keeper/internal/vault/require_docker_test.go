package vault

import "os"

// requireDocker — true, если CI требует обязательного docker-а
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Паттерн совпадает с
// keeper/internal/auditpg/require_docker_test.go; вынесен в без-теговый
// файл, чтобы integration_test.go под `//go:build integration` его видел,
// а отдельные unit-тесты на этом env-разборе оставались отдельно.
//
// Регрессионный test для парсинга env лежит в auditpg/require_docker_test.go;
// здесь дублировать не нужно — единая семантика.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
