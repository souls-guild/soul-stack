package reaper_test

import "os"

// requireDocker — true, если CI требует обязательного docker-а. Используется
// integration-tests-сетом для разрыва skip/fatal. Файл сознательно не под
// `//go:build integration` — пустой test-binary без build-tag не сломается
// (требует ровно нулевой коды на init), а с build-tag-ом — функция доступна.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
