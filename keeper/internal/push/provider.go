// Package push is the S0 pilot for keeper.push (agentless SSH delivery, ADR-004,
// docs/keeper/push.md). It implements a synchronous oneshot transport: Keeper SSHes
// into a `transport=ssh` host, runs `soul apply`, feeds the rendered
// ApplyRequest (protojson) into stdin, and reads the NDJSON stream of
// TaskEvents plus the final RunResult from stdout.
//
// The pilot proves the transport end-to-end and sets the pattern. Out of pilot
// scope (separate slices): SHA-256 delivery cache for the soul binary/modules
// (S1), vault-backed SSH CA provider (S2), integration into scenario-runner as
// an alt-Outbound dispatcher (S3), OpenAPI+MCP facades (S4), host-side cleanup (S5).
package push

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// SshProvider is the narrow SSH-authentication contract the dispatcher needs:
// Authorize (Keeper's right to reach a host) + Sign (issuing SSH credentials
// for a session). Its signatures match [pluginhost.SshProviderPlugin] (Sign /
// Authorize), so a real provider plugs in without an adapter, while tests mock
// it with a plain struct.
//
// The SshProvider contract is defined in sdk/sshprovider; this is the
// host-side consumer interface with exactly the two methods [SshDispatcher]
// uses. Narrowing the surface (rather than reusing all of
// pluginv1.SshProviderClient) keeps the dispatcher testable without spawning
// a plugin.
type SshProvider interface {
	// Authorize confirms Keeper's right to open an SSH session to (host, user).
	// A deny stops the run before connecting (fail-closed).
	Authorize(ctx context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error)
	// Sign issues SSH credentials for the current session (a CA-signed cert or
	// an ephemeral keypair, under a single contract).
	Sign(ctx context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error)
}
