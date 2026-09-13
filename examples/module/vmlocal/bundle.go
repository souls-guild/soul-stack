// The artifact's bundle — the objects `vmlocal` serves, and the schema document
// generated from them.
//
// The Go value is the source of truth: `soul-mod stamp` runs the artifact's own
// `schema` subcommand, checks the document, appends it to the binary as a trailer
// and writes the same bytes to `schema.json`. Nothing here is hand-written JSON,
// and `make check-plugin-schema` fails when the committed `schema.json` and this
// value disagree.
//
// The document carries NO name of its own — not the artifact's, not a namespace.
// Address level 1 is the alias an operator writes in
// `keeper.yml::plugins.soul_modules[].name`, so registering these bytes as
// `wbcloud` is what makes an unmodified cloud scenario address them.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// vmlocalBundle is the artifact: one object over one libvirt connection, because
// `vm` is the only thing a machine provider manages. Volumes and networks are
// looked up on the way to a VM, not managed in their own right, and an object an
// operator cannot address is not an object.
func vmlocalBundle(m *VMLocal) module.Bundle {
	return module.Bundle{
		Modules: []module.Def{
			vmDef(m),
		},
	}
}
