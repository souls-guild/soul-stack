package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/sshprovider"
	"golang.org/x/crypto/ssh"
)

// paramsEnv is the env var through which keeper.push passes params (JSON per
// schema.json) to the static provider when the plugin is forked. It mirrors
// handshake.SocketEnv: the SshProvider contract (Sign/Authorize) carries no
// per-request provider params, so config arrives at process startup, just like
// the socket path. This is the static-provider convention (the env name is
// scoped to this binary), NOT a generic params-delivery mechanism for all
// plugins; the latter would touch handshake/proto and belongs in a separate ADR.
const paramsEnv = "SOUL_SSH_STATIC_PARAMS"

// params is the static-provider configuration parsed from paramsEnv.
type params struct {
	// KeyPath is the path to the private SSH key on the keeper host (mutually
	// exclusive with VaultRef; oneOf in schema.json).
	KeyPath string `json:"key_path"`
	// VaultRef is a reference to the Vault KV secret containing the key. Vault
	// resolution is NOT implemented in the pilot (keeper.push resolves the secret
	// and substitutes key_path - A-flow, mirroring cloud credentials); the field is
	// kept here for schema completeness and the fail-closed branch.
	VaultRef string `json:"vault_ref"`
	// Deny is the deny-list of (host, user) pairs. Empty = allow-all
	// (dev/test default).
	Deny []denyRule `json:"deny"`
}

// denyRule is one deny-list entry. An empty field is a wildcard for that
// dimension (host:"" -> any host; user:"" -> any user). An entry with two empty
// fields denies everything - an explicit "close the provider" rule.
type denyRule struct {
	Host string `json:"host"`
	User string `json:"user"`
}

func (r denyRule) matches(host, user string) bool {
	return (r.Host == "" || r.Host == host) && (r.User == "" || r.User == user)
}

// StaticProvider is an SshProvider over a long-lived static key on the keeper
// host (ADR-016 dev/test and installations without Vault). Sign returns a ready
// pair (private_key from file, certificate=""); public_key from the request is
// ignored (static does not sign the client key). Authorize uses a deny-list and
// defaults to allow-all.
type StaticProvider struct {
	sshprovider.BaseProvider
	cfg params
}

// loadParams reads and validates params from env. Fail-closed: invalid JSON,
// missing key source, or vault_ref (not implemented in the pilot) returns an
// error and the plugin does not start.
func loadParams() (params, error) {
	raw := os.Getenv(paramsEnv)
	if raw == "" {
		return params{}, fmt.Errorf("env %s is empty: static provider requires params", paramsEnv)
	}
	var p params
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return params{}, fmt.Errorf("parse %s: %w", paramsEnv, err)
	}
	if p.VaultRef != "" && p.KeyPath == "" {
		// A-flow: keeper.push resolves Vault KV -> key_path before plugin startup
		// (like cloud credentials). Direct vault resolution inside the plugin is
		// outside the pilot.
		return params{}, errors.New("vault_ref is resolved by keeper.push into key_path before plugin startup (vault resolution in the plugin is outside the pilot)")
	}
	if p.KeyPath == "" {
		return params{}, errors.New("key source is not set: key_path is required (or vault_ref resolvable into key_path)")
	}
	return p, nil
}

// Sign reads the private key from cfg.KeyPath, checks that it is parseable
// (fail-closed: keeper.push calls ssh.ParsePrivateKey later, so it is better to
// fail here with a clear reason), and returns a ready pair. certificate="" means
// the static provider signs nothing.
func (s *StaticProvider) Sign(_ context.Context, _ *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	keyPEM, err := os.ReadFile(s.cfg.KeyPath)
	if err != nil {
		return nil, sshprovider.SignError(sshprovider.SignFailReadKey, fmt.Errorf("read %s: %w", s.cfg.KeyPath, err))
	}
	if _, perr := ssh.ParsePrivateKey(keyPEM); perr != nil {
		return nil, sshprovider.SignError(sshprovider.SignFailReadKey, fmt.Errorf("parse key %s: %w", s.cfg.KeyPath, perr))
	}
	return &pluginv1.SignReply{
		Certificate: "",
		PrivateKey:  string(keyPEM),
		// The static key is long-lived: 0 = "no refresh deadline" (keeper.push does
		// not schedule rotation for static, unlike CA providers).
		TtlSeconds: 0,
	}, nil
}

// Authorize applies the deny-list. Empty deny -> allow-all (dev/test). A match
// with any rule returns deny with a reason from the SDK dictionary (for reason
// aggregation on the Keeper side).
func (s *StaticProvider) Authorize(_ context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	for _, rule := range s.cfg.Deny {
		if rule.matches(req.GetHost(), req.GetUser()) {
			return &pluginv1.AuthorizeReply{
				Allowed: false,
				Reason:  sshprovider.DenyMessage(sshprovider.DenyExplicitDeny, req.GetUser()+"@"+req.GetHost()),
			}, nil
		}
	}
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}
