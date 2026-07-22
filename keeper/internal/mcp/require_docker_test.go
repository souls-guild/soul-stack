//go:build integration

package mcp

import "os"

func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
