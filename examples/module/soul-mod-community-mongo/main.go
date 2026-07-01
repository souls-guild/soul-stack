// Entry-point плагина soul-mod-community-mongo. Собирается в один статический
// бинарь через `go build`; Soul запускает его как sub-process, делает
// gRPC-stdio handshake (sdk/handshake) и зовёт RPC SoulModule. Логика — impl.go.
package main

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/sdk/module"
)

func main() {
	if err := module.Serve(&MongoModule{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-mod-community-mongo:", err)
		os.Exit(1)
	}
}
