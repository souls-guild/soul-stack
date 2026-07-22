package push

import (
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"golang.org/x/crypto/ssh"
)

// NewEphemeralEd25519 — exported wrapper around [newEphemeralEd25519] for
// reuse outside the push package (keeper-side core module
// `core.bootstrap.delivered`, ADR-063: same ephemeral-keypair + Sign-flow as
// SshDispatcher.SendApply). Generates a fresh ed25519 keypair per session;
// the private key stays ONLY inside the returned signer.
func NewEphemeralEd25519() (ssh.Signer, string, error) {
	return newEphemeralEd25519()
}

// AuthMethodsFromSign — exported wrapper around [authMethodsFromSign]:
// converts a SignReply (from SshProvider) into ssh.AuthMethod values
// (ephemeral-cert or static-key mode). Reused by `core.bootstrap.delivered`
// (ADR-063) the same way as SshDispatcher.
func AuthMethodsFromSign(reply *pluginv1.SignReply, ephSigner ssh.Signer) ([]ssh.AuthMethod, error) {
	return authMethodsFromSign(reply, ephSigner)
}
