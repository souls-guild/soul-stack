//go:build race

package integrationenv

// RaceEnabled reports whether this binary was built with the race detector.
// The value is decided at build time by the `race` tag the toolchain sets for
// `go test -race`; there is no way to ask at runtime otherwise.
const RaceEnabled = true
