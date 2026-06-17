// soul-ssh-vault — реальный SshProvider-плагин Soul Stack для Vault SSH CA.
//
// Канонический режим работы — Keeper-ephemeral (PM-decision SSH key-ownership):
//
//  1. Keeper генерит ephemeral ed25519 keypair per-session и шлёт публичную
//     часть в `SignRequest.public_key` (см. keeper/internal/push/dispatcher.go).
//  2. Плагин аутентифицируется в Vault (auth_method: token / approle), вызывает
//     `ssh/sign/<role>` с этой pubkey и возвращает `SignReply{certificate=<signed>,
//     private_key=""}`. Приватник НИКОГДА не покидает Keeper.
//  3. Keeper собирает [ssh.NewCertSigner] из ephSigner + cert и открывает
//     SSH-сессию.
//
// Собирается в статический бинарь `soul-ssh-vault` (конвенция dist/<binary>).
// Params (vault_addr / role / auth-credentials / deny-list) приезжают на
// старте через env `SOUL_SSH_VAULT_PARAMS` — симметрично soul-ssh-static (см.
// impl.go → paramsEnv). SshProvider-контракт не несёт per-request параметров
// провайдера, поэтому конфиг едет как и путь к сокету (handshake.SocketEnv).
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
