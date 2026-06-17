// soul-ssh-teleport — реальный SshProvider-плагин Soul Stack для Teleport SSH.
//
// Канонический режим работы — Keeper-ephemeral (PM-decision SSH key-ownership):
//
//  1. Keeper генерит ephemeral ed25519 keypair per-session и шлёт публичную
//     часть в `SignRequest.public_key` (см. keeper/internal/push/dispatcher.go).
//  2. Плагин аутентифицируется в Teleport (creds-flow B): identity-file или
//     tbot-сокет, через env `SOUL_SSH_TELEPORT_PARAMS`. Вызывает Teleport Auth
//     `GenerateUserCerts(req.PublicKey)` → возвращает signed-cert на keeper
//     pubkey + endpoint Teleport-proxy в SignReply.proxy_jump.
//  3. SignReply: {certificate=<signed_ssh_cert>, private_key="",
//     proxy_jump=<proxy_addr>}. Приватник НИКОГДА не покидает Keeper.
//
// ВАЖНО: dispatcher proxy_jump support — ОТДЕЛЬНЫЙ слайс (S3 после пилота).
// На момент пилота `keeper.push` ИГНОРИРУЕТ SignReply.proxy_jump и ходит
// прямым `net.Dial(host:port)` — поэтому Teleport-flow работает только на
// хосты с прямой доступностью без bastion-а (см. docs/keeper/plugins.md →
// kind: ssh_provider (Teleport)). Полный Teleport-через-proxy требует
// dispatcher proxy_jump implementation.
//
// Собирается в статический бинарь `soul-ssh-teleport` (конвенция dist/<binary>).
// Params (proxy_addr / identity_file|tbot_socket / roles / deny-list) приезжают
// на старте через env `SOUL_SSH_TELEPORT_PARAMS` — симметрично soul-ssh-vault и
// soul-ssh-static (SshProvider-контракт не несёт per-request параметров
// провайдера, поэтому конфиг едет как и путь к сокету handshake.SocketEnv).
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
