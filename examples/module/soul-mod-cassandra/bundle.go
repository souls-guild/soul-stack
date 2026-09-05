// The artifact's bundle — the five objects `soul-mod-cassandra` serves, and the
// schema document generated from them.
//
// The Go value is the source of truth (NIM-377): `soul-mod stamp` runs the artifact's
// own `schema` subcommand, checks the document, appends it to the binary as a trailer
// and writes the same bytes to `dist/schema.json`. Nothing here is hand-written JSON,
// and `make check-plugin-schema` fails if the committed `schema.json` and this value
// disagree (NIM-525).
//
// The document carries NO name of its own — not the artifact's, not a namespace.
// Address level 1 is the alias an operator writes in `keeper.yml::plugins.*[].name`
// and level 2 is a module name below, so the same bytes registered as `cassandra` and
// as `cassandra-community` answer at two address spaces (ADR-020(p), NIM-377).
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// cassandraBundle is the artifact: five objects over one CQL driver.
//
// The order is alphabetical and that is load-bearing — `modules` is a JSON array, so
// the canonical bytes keep whatever order this slice has, and they are hashed and
// signed.
func cassandraBundle(m *CassandraModule) module.Bundle {
	return module.Bundle{
		Modules: []module.Def{
			commandDef(m),
			instanceDef(m),
			keyspaceDef(m),
			roleDef(m),
			tableDef(m),
		},
	}
}
