// Entry-point of the vmlocal plugin: a machine provider over libvirt/QEMU, so
// that work needing real machines can be done against a host you already have
// instead of a billed cloud VM per cycle.
//
// Collected into one static binary via `go build` — the libvirt bindings are pure
// Go (digitalocean/go-libvirt speaks the RPC protocol itself), so CGO_ENABLED=0
// holds and the artifact a Keeper fetches has no shared-library tail.
//
// [module.ServeBundle] dispatches on argv[1]: an object name (`vmlocal vm`) serves
// that object over gRPC, and `schema` prints the artifact's own document, which is
// how `soul-mod stamp` derives what it stamps.
package main

import (
	"github.com/souls-guild/soul-stack/sdk/module"
)

func main() {
	module.ServeBundle(vmlocalBundle(&VMLocal{}))
}
