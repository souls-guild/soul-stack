// soul-ssh-vault is a real Soul Stack SshProvider plugin for Vault SSH CA.
//
// Canonical operation mode is Keeper-ephemeral (PM-decision SSH key-ownership):
//
//  1. Keeper generates an ephemeral ed25519 keypair per session and sends the
//     public part in `SignRequest.public_key` (see keeper/internal/push/dispatcher.go).
//  2. The plugin authenticates to Vault (auth_method: token / approle), calls
//     `ssh/sign/<role>` with this pubkey, and returns `SignReply{certificate=<signed>,
//     private_key=""}`. The private key NEVER leaves Keeper.
//  3. Keeper builds [ssh.NewCertSigner] from ephSigner + cert and opens the SSH
//     session.
//
// Builds into the static binary `soul-ssh-vault` (dist/<binary> convention).
// Params (vault_addr / role / auth-credentials / deny-list) arrive at startup
// through env `SOUL_SSH_VAULT_PARAMS` - symmetrical with soul-ssh-static (see
// impl.go -> paramsEnv). The SshProvider contract carries no per-request
// provider params, so config travels like the socket path (handshake.SocketEnv).
package main

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/sdk/sshprovider"
)

func main() {
	cfg, err := loadParams()
	if err != nil {
		fmt.Fprintln(os.Stderr, "soul-ssh-vault:", err)
		os.Exit(1)
	}
	if err := sshprovider.Serve(&VaultProvider{cfg: cfg}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-ssh-vault:", err)
		os.Exit(1)
	}
}
