// Entry point of the soul-mod-community-mongo plugin. Built into a single static
// binary with `go build`; Soul starts it as a subprocess, performs gRPC-stdio
// handshake (sdk/handshake), and calls SoulModule RPC. Logic lives in impl.go.
package main

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/sdk/module"
)

func main() {
	if err := module.Serve(&MongoModule{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-mod-community-mongo:", err)
		os.Exit(1)
	}
}
