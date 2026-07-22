package push

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

// hostCAField — the Vault KV field name holding the PEM-encoded SSH public
// key for the host-CA (`push.host_ca_ref` / `push.host_ca_refs[].ref`).
// Symmetric with `signing_key` (auth.jwt) / `password` (metrics.auth.basic):
// the Vault KV secret holds exactly one field, its name fixed by convention.
const hostCAField = "public_key"

// NamedHostKeyAuthority — one CA in the multi-CA set (S7-3, ADR-032 amendment
// 2026-05-26). Name is operator-defined (or the `default` auto-name under
// backward-compat adapt of the singular `host_ca_ref`); used as the label
// value in `keeper_push_host_ca_used_total{ca_name=...}` and in diagnostic
// messages.
//
// SourceRef is kept for logs / fail-fast errors (without it, a "PEM parse
// failed" error gives no way to tell which ref is broken).
type NamedHostKeyAuthority struct {
	Name      string
	CAPubKey  ssh.PublicKey
	SourceRef string
}

// KVReader — the narrow Vault KV read needed by [LoadHostCA]. Satisfied by
// the keeper-side *keepervault.Client; factored out separately for unit
// testability without standing up a Vault client.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// LoadHostCA reads the public host-CA from Vault by a vault ref
// (`vault:<mount>/<path>`) and returns a [HostKeyAuthority] for injection
// into [SshDispatcher.Deps.HostAuthority].
//
// The Vault KV secret must have a `public_key` field with one of:
//   - OpenSSH authorized_keys form (`ssh-ed25519 AAAA... [comment]`);
//   - a PEM SSH PUBLIC KEY block (supported via `ssh.ParseAuthorizedKey` for
//     authorized forms; the PEM wrapper isn't covered by the pilot schema,
//     the operator puts in an authorized-key, the most common output form of
//     `ssh-keygen -y`).
//
// Any resolve error (missing ref / Vault unavailable / missing field /
// invalid form) is fail-fast: the caller (daemon setupPushDispatchers) aborts
// keeper startup, otherwise the push dispatcher would run without a host-CA
// and any connect would fail with a confusing error.
func LoadHostCA(ctx context.Context, vc KVReader, ref string) (HostKeyAuthority, error) {
	if vc == nil {
		return HostKeyAuthority{}, errors.New("push: LoadHostCA: vault client is nil")
	}
	if ref == "" {
		return HostKeyAuthority{}, errors.New("push: LoadHostCA: empty vault ref")
	}
	path, err := keepervault.ParseRef(ref)
	if err != nil {
		return HostKeyAuthority{}, fmt.Errorf("push: LoadHostCA: %w", err)
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return HostKeyAuthority{}, fmt.Errorf("push: LoadHostCA: read vault %q: %w", path, err)
	}
	raw, ok := kv[hostCAField]
	if !ok {
		return HostKeyAuthority{}, fmt.Errorf("push: LoadHostCA: vault secret %q has no %q field", path, hostCAField)
	}
	pemStr, ok := raw.(string)
	if !ok || pemStr == "" {
		return HostKeyAuthority{}, fmt.Errorf("push: LoadHostCA: vault field %q is empty or not a string", hostCAField)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pemStr))
	if err != nil {
		return HostKeyAuthority{}, fmt.Errorf("push: LoadHostCA: parse host-CA public key: %w", err)
	}
	return HostKeyAuthority{CAPublicKey: pub}, nil
}

// LoadHostCAs resolves a set of vault refs into a [NamedHostKeyAuthority] set
// for multi-CA host-key verification (S7-3, ADR-032 amendment 2026-05-26).
// Reuses [LoadHostCA] for each element — the single point of Vault KV
// reading / PEM parsing. If resolving one of the refs fails, it's fail-fast
// with the CA name in the wrapper (caller `setupPushDispatchers` aborts
// keeper startup).
//
// An empty `refs` → nil, nil: the caller decides whether that's a failure or
// a valid case (singular path / push disabled). The daemon aborts startup
// earlier, at the "host_ca_ref/refs not set" gate check.
func LoadHostCAs(ctx context.Context, vc KVReader, refs []config.KeeperPushCARef) ([]NamedHostKeyAuthority, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if vc == nil {
		return nil, errors.New("push: LoadHostCAs: vault client is nil")
	}
	out := make([]NamedHostKeyAuthority, 0, len(refs))
	for _, r := range refs {
		ca, err := LoadHostCA(ctx, vc, r.Ref)
		if err != nil {
			return nil, fmt.Errorf("push: LoadHostCAs[%s]: %w", r.Name, err)
		}
		out = append(out, NamedHostKeyAuthority{
			Name:      r.Name,
			CAPubKey:  ca.CAPublicKey,
			SourceRef: r.Ref,
		})
	}
	return out, nil
}
