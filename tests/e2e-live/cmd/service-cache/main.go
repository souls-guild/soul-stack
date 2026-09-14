// Command service-cache fills the L3b cache of out-of-tree service repositories, at the
// commits tests/e2e-live/harness/servicecatalog.go pins (NIM-876).
//
// A service is its own repository (NIM-871), so the tier's real subject is not in this
// tree. Cloning it per run would make a blocking pre-tag gate depend on github.com being
// up; reading a checkout on disk would make it prove whatever happened to be in that
// directory. A pinned commit in a cache is neither, and this is what fills the cache —
// once per machine per pin bump, named, up front, instead of inside a test.
//
// Run it via `make e2e-live-services`. It is also the deliberate way to prepare a machine
// that is about to go offline: with the cache warm, SOUL_STACK_E2E_SERVICE_OFFLINE=1 turns
// a missing pin into a loud error instead of a silent fetch.
//
// `-check-overrides` is the gate's question, not a user's: it exits non-zero when a pin is
// overridden by a working tree, because a gate run under that override reports on a
// directory nobody can name.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func main() {
	checkOverrides := flag.Bool("check-overrides", false,
		"exit non-zero if a pinned service is overridden by a working tree")
	flag.Parse()

	if *checkOverrides {
		over := harness.OverriddenServiceDirs()
		if len(over) == 0 {
			return
		}
		aliases := make([]string, 0, len(over))
		for alias := range over {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		fmt.Fprintln(os.Stderr, "service-cache: a pinned service is overridden by a working tree:")
		for _, alias := range aliases {
			fmt.Fprintf(os.Stderr, "  %s -> %s\n", alias, over[alias])
		}
		fmt.Fprintln(os.Stderr,
			"  The gate's verdict is about a pinned commit. Under this override it would be about\n"+
				"  an unnamed directory instead — which is the failure the pin exists to prevent.\n"+
				"  Unset it for the gate; a hand-run `go test -tags=e2e_live` still honours it.")
		os.Exit(1)
	}

	dir, err := harness.PrimeServiceCache()
	if err != nil {
		fmt.Fprintf(os.Stderr, "service-cache: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("service-cache: ready in %s\n", dir)
}
