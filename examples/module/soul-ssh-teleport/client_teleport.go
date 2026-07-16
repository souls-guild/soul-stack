package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	tpclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
)

// realTeleportClient is the production teleportClient implementation over
// github.com/gravitational/teleport/api/client. Narrow wrapper: only
// GenerateUserCerts (minimal surface for Sign) + Close. It does not recursively
// pull the teleport-server codebase: module `teleport/api` is standalone
// (Gravitational split it out specifically for plugin scenarios).
type realTeleportClient struct {
	c *tpclient.Client
	// defaultTTL is the TTL of the requested user-cert (the TTL of the Teleport
	// role assigned to identity/bot caps it from above).
	defaultTTL time.Duration
	// clusterName is RouteToCluster (multi-cluster trust). Empty = current
	// identity-file cluster.
	clusterName string
}

// defaultClient is the production teleportClient factory: opens a gRPC
// connection to Teleport Auth through Teleport proxy by identity-file / tbot
// socket.
//
// Creds-flow B (PM-decision): the plugin authenticates itself.
//   - identity_file -> tpclient.LoadIdentityFile (issued by `tctl auth sign`).
//   - tbot_socket   -> tpclient.LoadIdentityFile over a renewable bundle that
//     tbot writes to its dest (behavior is compatible with identity-file format -
//     tbot writes the same format). Real tbot deployments usually point to the
//     same directory; full native-tbot connection through unix socket is an
//     extension (not pilot).
//
// Returns: client + nil -> ok; nil + err -> fail (the Sign branch wraps it in
// SignFailIssue).
func defaultClient(ctx context.Context, p params) (teleportClient, error) {
	if p.IdentityFile == "" && p.TbotSocket == "" {
		// loadParams already checked this, but for defense-in-depth the factory
		// must not start silently without credentials.
		return nil, errors.New("no credentials: identity_file/tbot_socket are empty")
	}
	credPath := p.IdentityFile
	if credPath == "" {
		credPath = p.TbotSocket
	}
	creds := tpclient.LoadIdentityFile(credPath)

	c, err := tpclient.New(ctx, tpclient.Config{
		Addrs:       []string{p.ProxyAddr},
		Credentials: []tpclient.Credentials{creds},
	})
	if err != nil {
		return nil, fmt.Errorf("teleport client: %w", err)
	}
	return &realTeleportClient{
		c:           c,
		defaultTTL:  12 * time.Hour, // the Teleport role cap still narrows this
		clusterName: p.ClusterName,
	}, nil
}

func (r *realTeleportClient) GenerateUserSSHCert(ctx context.Context, pubkey, principal string, roles []string) (string, error) {
	resp, err := r.c.GenerateUserCerts(ctx, proto.UserCertsRequest{
		// UserCertsRequest contains a PAIR of keys (SSH + TLS) for future
		// combined-cert scenarios; for pure SSH-flow, fill only SSHPublicKey and
		// leave TLSPublicKey empty - Teleport returns only an SSH-cert for this
		// combination (see lib/auth GenerateUserCerts).
		SSHPublicKey:   []byte(pubkey),
		Username:       principal,
		Expires:        time.Now().Add(r.defaultTTL),
		RouteToCluster: r.clusterName,
	})
	if err != nil {
		return "", err
	}
	if resp == nil || len(resp.SSH) == 0 {
		return "", errors.New("teleport: empty SSH cert in response")
	}
	_ = roles // Teleport applies identity roles; additional roles require RoleRequests on the cert request - deferred (not pilot).
	return string(resp.SSH), nil
}

func (r *realTeleportClient) Close() error {
	if r.c == nil {
		return nil
	}
	return r.c.Close()
}
