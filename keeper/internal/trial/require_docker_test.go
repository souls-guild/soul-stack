//go:build integration

package trial

import "os"

// requireDocker — true, если CI требует обязательного docker-а
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Паттерн совпадает с прочими
// integration-тестами keeper (topology/applyrun/...): без флага и без docker —
// тест skip; с флагом — fatal при недоступном docker.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
