// Entry-point of the soul-mod-community-redis plugin. Collected into one static
// binary via `go build`; Soul runs it as a sub-process, does
// gRPC-stdio handshake (sdk/handshake) and calls RPC SoulModule. Logic - impl.go.
package main

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/sdk/module"
)

func main() {
	if err := module.Serve(&RedisModule{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-mod-community-redis:", err)
		os.Exit(1)
	}
}
