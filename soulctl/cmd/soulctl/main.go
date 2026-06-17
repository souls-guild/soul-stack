// soulctl — клиентский CLI оператора Soul Stack, тонкая обёртка над Operator API
// Keeper-а.
//
// Этот entrypoint только собирает корневую команду (cmd.NewRoot) и запускает её.
// Дерево подкоманд — в internal/cmd: группы incarnation / souls / soul / errand /
// archon / push-providers / run.
package main

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/soulctl/internal/cmd"
)

// soulctlVersion — версия бинаря, инжектится через -ldflags '-X ...soulctlVersion=...'
// (см. Makefile, симметрия с soulVersion). На голой сборке без ldflags = "0.0.0-dev".
var soulctlVersion = "0.0.0-dev"

func main() {
	root := cmd.NewRoot(soulctlVersion)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
