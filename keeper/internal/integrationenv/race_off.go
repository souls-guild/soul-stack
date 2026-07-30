//go:build !race

package integrationenv

// RaceEnabled reports whether this binary was built with the race detector.
// See race_on.go — this is the other half of the build-tag pair.
const RaceEnabled = false
