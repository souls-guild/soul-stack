package reaper_test

import "os"

// requireDocker is true when CI requires Docker. It is used by the integration
// test set to choose skip vs fatal. The file is intentionally not under
// `//go:build integration`: an empty test binary without the build tag will not
// break, and with the build tag the function is available.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
