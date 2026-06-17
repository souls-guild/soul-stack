// soul-ssh-static — реальный SshProvider-плагин Soul Stack: static-key
// (ADR-016 dev/test и инсталляции без Vault; пилот тиража SshProvider по
// docs/keeper/plugins.md). Reference-реализация для последующих провайдеров
// (Vault SSH CA, Teleport — отдельный тираж).
//
// Собирается в статический бинарь `soul-ssh-static` (конвенция dist/<binary> —
// docs/keeper/plugins.md). Keeper-side модуль `keeper.push` перед SSH-сессией
// запускает его как sub-process, делает gRPC-stdio handshake (sdk/handshake) и
// зовёт RPC SshProvider: Authorize (право ходить на host) → Sign (выдать пару).
//
// Params (key_path / deny-list, schema.json) приезжают на старте через env
// SOUL_SSH_STATIC_PARAMS — SshProvider-контракт не несёт per-request параметров
// провайдера (см. impl.go → paramsEnv). Shared-каркас reason-кодов (deny / sign-
// fail для аудита) — из sdk/sshprovider, общий для тиража.
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
