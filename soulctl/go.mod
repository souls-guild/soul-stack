module github.com/souls-guild/soul-stack/soulctl

go 1.26.4

require (
	github.com/goccy/go-yaml v1.19.2
	github.com/souls-guild/soul-stack/shared v0.0.0
	github.com/spf13/cobra v1.10.2
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
)

// shared/ carries the Operator API wire types (shared/api/wire) - the ONE
// declaration of every request/reply body, which soulctl used to restate by
// hand and silently diverge from (NIM-776). Only that package is imported, so
// nothing of shared/'s server-side dependency graph is linked in. shared/ and
// its transitive project dependency proto/plugin are private modules with no
// proxy publication, so each needs an explicit replace to a local path,
// otherwise `go mod tidy` hits a 404 on sum.golang.org (same as tests/e2e).
replace (
	github.com/souls-guild/soul-stack/proto/plugin => ../proto/plugin
	github.com/souls-guild/soul-stack/sdk => ../sdk
	github.com/souls-guild/soul-stack/shared => ../shared
)
