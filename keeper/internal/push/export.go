package push

import (
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"golang.org/x/crypto/ssh"
)

// NewEphemeralEd25519 — exported-обёртка над [newEphemeralEd25519] для
// переиспользования вне пакета push (keeper-side core-модуль
// `core.bootstrap.delivered`, ADR-063: тот же ephemeral-keypair + Sign-flow, что
// у SshDispatcher.SendApply). Генерирует свежий ed25519-keypair per-session;
// приватник остаётся ТОЛЬКО внутри возвращённого signer-а.
func NewEphemeralEd25519() (ssh.Signer, string, error) {
	return newEphemeralEd25519()
}

// AuthMethodsFromSign — exported-обёртка над [authMethodsFromSign]: конвертирует
// SignReply (от SshProvider) в ssh.AuthMethod-ы (ephemeral-cert либо static-key
// режим). Переиспользуется `core.bootstrap.delivered` (ADR-063) тем же путём,
// что SshDispatcher.
func AuthMethodsFromSign(reply *pluginv1.SignReply, ephSigner ssh.Signer) ([]ssh.AuthMethod, error) {
	return authMethodsFromSign(reply, ephSigner)
}
