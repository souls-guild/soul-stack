// Command artifact-cache primes the L3b release-tarball cache (NIM-542).
//
// The L3b tier used to fetch three GitHub release tarballs from inside the soul
// container on every live create — six of the then-nine gate tests, ~18 downloads
// per gate run. The gate is the pre-tag blocking step, and its acceptance is "three
// runs on an unchanged slice give the same result"; resting that on github.com
// being up meant resting it on something that is not in the slice. Now the harness
// serves those tarballs from a local mirror, and this is what fills it.
//
// Run it via `make e2e-live-artifacts`. The name is descriptive rather than drawn
// from the Soul Stack dictionary on purpose: this is test scaffolding, not a
// product artifact, and a dictionary name would be a new name to defend.
package main

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func main() {
	dir, err := harness.PrimeArtifactCache()
	if err != nil {
		fmt.Fprintf(os.Stderr, "artifact-cache: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("artifact-cache: ready in %s\n", dir)
}
