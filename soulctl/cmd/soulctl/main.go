// soulctl is the Soul Stack operator's client CLI, a thin wrapper over
// Keeper's Operator API.
//
// This entrypoint only assembles the root command (cmd.NewRoot) and runs it.
// The subcommand tree lives in internal/cmd: the incarnation / souls / soul /
// errand / archon / push-providers / run groups.
package main

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/soulctl/internal/cmd"
)

// soulctlVersion is the binary version, injected via
// -ldflags '-X ...soulctlVersion=...' (see Makefile, symmetric with
// soulVersion). On a bare build without ldflags it's "0.0.0-dev".
var soulctlVersion = "0.0.0-dev"

func main() {
	root := cmd.NewRoot(soulctlVersion)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
