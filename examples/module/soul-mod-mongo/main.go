// Entry-point of the soul-mod-mongo plugin. Collected into one static binary via
// `go build`; the host runs it as a sub-process, does the gRPC-stdio handshake
// (sdk/handshake) and calls RPC SoulModule. Logic - impl.go.
//
// [module.ServeBundle] dispatches on argv[1]: an object name (`soul-mod-mongo
// user`) serves that object over gRPC, and `schema` prints the artifact's own
// document, which is how `soul-mod stamp` derives what it stamps. The single-module
// [module.Serve] this used to call served neither, and it ignored argv[1] outright —
// one module worked by accident, three cannot (NIM-769, the shape of NIM-525).
package main

import (
	"github.com/souls-guild/soul-stack/sdk/module"
)

func main() {
	module.ServeBundle(mongoBundle(&MongoModule{}))
}
