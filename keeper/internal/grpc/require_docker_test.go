package grpc

import "os"

// requireDocker — true if CI requires Docker to be mandatory
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Pattern matches
// keeper/internal/vault/require_docker_test.go.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}
