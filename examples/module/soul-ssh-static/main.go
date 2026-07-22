// soul-ssh-static is a real Soul Stack SshProvider plugin: static-key
// (ADR-016 dev/test and installations without Vault; SshProvider rollout pilot
// per docs/keeper/plugins.md). Reference implementation for later providers
// (Vault SSH CA, Teleport - separate rollout).
//
// Builds into the static binary `soul-ssh-static` (dist/<binary> convention -
// docs/keeper/plugins.md). Before the SSH session, the Keeper-side `keeper.push`
// module starts it as a sub-process, performs the gRPC-stdio handshake
// (sdk/handshake), and calls SshProvider RPCs: Authorize (permission to access a
// host) -> Sign (issue a key pair).
//
// Params (key_path / deny-list, schema.json) arrive at startup through the
// SOUL_SSH_STATIC_PARAMS env var - the SshProvider contract carries no
// per-request provider params (see impl.go -> paramsEnv). The shared framework
// of reason codes (deny / sign-fail for audit) comes from sdk/sshprovider and is
// common for the rollout.
package main

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/sdk/sshprovider"
)

func main() {
	cfg, err := loadParams()
	if err != nil {
		fmt.Fprintln(os.Stderr, "soul-ssh-static:", err)
		os.Exit(1)
	}
	if err := sshprovider.Serve(&StaticProvider{cfg: cfg}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-ssh-static:", err)
		os.Exit(1)
	}
}
