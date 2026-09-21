package push

import (
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"golang.org/x/crypto/ssh"
)

// NewEphemeralEd25519 — exported wrapper around [newEphemeralEd25519] for
// reuse outside the push package (the keeper-side core module `core.ssh.run`,
// NIM-849: the same ephemeral-keypair + Sign flow as SshDispatcher.SendApply).
// A fresh ed25519 keypair per session; the private key stays ONLY inside the
// returned signer and never leaves the Keeper.
func NewEphemeralEd25519() (ssh.Signer, string, error) {
	return newEphemeralEd25519()
}

// AuthMethodsFromSign — exported wrapper around [authMethodsFromSign]:
// converts a SignReply (from SshProvider) into ssh.AuthMethod values
// (ephemeral-cert or static-key mode). Reused by `core.ssh.run` the same way
// as SshDispatcher.
func AuthMethodsFromSign(reply *pluginv1.SignReply, ephSigner ssh.Signer) ([]ssh.AuthMethod, error) {
	return authMethodsFromSign(reply, ephSigner)
}
