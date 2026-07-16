// soul-ssh-teleport is a real Soul Stack SshProvider plugin for Teleport SSH.
//
// Canonical operation mode is Keeper-ephemeral (PM-decision SSH key-ownership):
//
//  1. Keeper generates an ephemeral ed25519 keypair per session and sends the
//     public part in `SignRequest.public_key` (see keeper/internal/push/dispatcher.go).
//  2. The plugin authenticates to Teleport (creds-flow B): identity-file or
//     tbot socket through env `SOUL_SSH_TELEPORT_PARAMS`. It calls Teleport Auth
//     `GenerateUserCerts(req.PublicKey)` -> returns a signed-cert for the keeper
//     pubkey + Teleport-proxy endpoint in SignReply.proxy_jump.
//  3. SignReply: {certificate=<signed_ssh_cert>, private_key="",
//     proxy_jump=<proxy_addr>}. The private key NEVER leaves Keeper.
//
// IMPORTANT: dispatcher proxy_jump support is a SEPARATE slice (S3 after the
// pilot). During the pilot, `keeper.push` IGNORES SignReply.proxy_jump and uses
// direct `net.Dial(host:port)`, so Teleport-flow works only for hosts with direct
// reachability without a bastion (see docs/keeper/plugins.md -> kind:
// ssh_provider (Teleport)). Full Teleport-through-proxy requires dispatcher
// proxy_jump implementation.
//
// Builds into the static binary `soul-ssh-teleport` (dist/<binary> convention).
// Params (proxy_addr / identity_file|tbot_socket / roles / deny-list) arrive at
// startup through env `SOUL_SSH_TELEPORT_PARAMS` - symmetrical with
// soul-ssh-vault and soul-ssh-static (the SshProvider contract carries no
// per-request provider params, so config travels like handshake.SocketEnv).
package main

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/sdk/sshprovider"
)

func main() {
	cfg, err := loadParams()
	if err != nil {
		fmt.Fprintln(os.Stderr, "soul-ssh-teleport:", err)
		os.Exit(1)
	}
	if err := sshprovider.Serve(&TeleportProvider{cfg: cfg}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-ssh-teleport:", err)
		os.Exit(1)
	}
}
