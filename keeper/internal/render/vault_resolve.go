package render

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// vaultRefPrefix — the marker for a Vault KV reference string
// (`vault:<mount>/<path>`). Matches the form parsed by vault.ParseRef.
const vaultRefPrefix = "vault:"

// resolveVaultRefs — the first pipeline phase (vault-resolve, [ADR-010]).
// Recursively walks a task's params and replaces every `vault:` ref string with
// the value read from Vault KV. No-op if there are no refs (PM-decision 2): the
// walk is cheap, and no extra Vault round-trip happens when there are no refs.
//
// Returns a new structure (the source isn't mutated): the orchestrator can render
// the same scenario again (retry), with fresh Vault values on every run.
//
// Errors:
//   - a `${ … }` marker inside a vault-ref → validation error: a vault-ref must be
//     a static string, interpolation in it is ambiguous (resolve ${} before or
//     after reading Vault?) and forbidden ([ADR-010], phase boundary).
//   - an unknown path / Vault transport error → propagated as-is
//     (vault.ErrVaultKVNotFound / wrapped).
func resolveVaultRefs(ctx context.Context, vc KVReader, params map[string]any) (map[string]any, error) {
	if len(params) == 0 {
		return params, nil
	}
	out, err := walkVaultValue(ctx, vc, params)
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]any)
	return m, nil
}

// walkVaultValue recursively resolves vault-refs in an arbitrary YAML value
// (map / slice / scalar). Returns a new value of the same kind.
func walkVaultValue(ctx context.Context, vc KVReader, v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := walkVaultValue(ctx, vc, val)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			rv, err := walkVaultValue(ctx, vc, val)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	case string:
		if !strings.HasPrefix(t, vaultRefPrefix) {
			return t, nil
		}
		return readVaultRef(ctx, vc, t)
	default:
		return v, nil
	}
}

// readVaultRef parses a `vault:` ref string and reads the corresponding secret.
//
// Ref form: `vault:<mount>/<path>` (vault.ParseRef) with an optional `#<field>`
// suffix — selecting one key from the KV secret. Without the suffix, the whole
// secret map is returned (downstream CEL extracts the needed field).
func readVaultRef(ctx context.Context, vc KVReader, ref string) (any, error) {
	if strings.Contains(ref, "${") {
		return nil, fmt.Errorf("render: vault-ref %q contains a ${...} marker -- a vault-ref must be a static string ([ADR-010], phase boundary)", ref)
	}
	if vc == nil {
		return nil, fmt.Errorf("render: vault-ref %q found, but the Vault client is not configured", ref)
	}

	body := ref
	var field string
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		body, field = ref[:i], ref[i+1:]
		if field == "" {
			return nil, fmt.Errorf("render: vault-ref %q: empty field name after '#'", ref)
		}
	}

	logical, err := vault.ParseRef(body)
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	// Through the per-render-pass memo, not vc.ReadKV directly: this phase and CEL
	// vault() read the same secrets in the same pass (a password referenced by a
	// `vault:` param and by ${ vault(...) } is one path), and a pass that caches on
	// one route only is both slower and no longer a single point-in-time view. The
	// memo is bound on ctx by Pipeline.Render; without it this degrades to a plain
	// ReadKV (soul-lint/Trial/unit-eval), same as the CEL side.
	data, err := cel.ReadKVMemoized(ctx, vc, logical)
	if err != nil {
		// NIM-73: the path in FLAT form (logical, without the `vault:` prefix) is
		// actionable diagnostics that survives observability masking
		// (audit.vaultRefRe only catches `vault:<mount>/`). A not-found secret has no
		// value → no leak. Symmetric with shared/cel.callVault. `%w` preserves the
		// ErrVaultKVNotFound chain.
		if field != "" {
			return nil, fmt.Errorf("render: secret %s#%s failed to resolve: %w", logical, field, err)
		}
		return nil, fmt.Errorf("render: secret %s failed to resolve: %w", logical, err)
	}

	if field == "" {
		return data, nil
	}
	val, ok := data[field]
	if !ok {
		// Path+field name is actionable (which field to add), not secret value
		// (other fields' values don't go into the text). Flat form (NIM-73).
		return nil, fmt.Errorf("render: secret %s has no field %q", logical, field)
	}
	return val, nil
}

// resolveRegisterSecrets resolves the `vault:` references a keeper-side module put
// in its register ([ADR-0083] §6) and returns the resolved bucket. The source map
// is not mutated: the same RenderInput is rendered again per passage and per retry.
//
// Why the register carries references at all: `core.state.*` writes the secret
// to Vault and hands back `vault:<path>#<field>`, so the plaintext never enters
// `apply_task_register` (which is durable and only reaped by age) and never reaches
// an audit payload. The consumer still needs the VALUE — the Soul-side task that
// writes users.acl — so the reference is resolved here, at the register→CEL-root
// boundary, on the way into the render context.
//
// TWO conditions, both required, and the second is the load-bearing one:
//
//  1. Only the KEEPER bucket. Per-host buckets are register results reported by
//     Souls. A compromised host that put the string
//     `vault:secret/keeper/jwt-signing-key#key` in a register would otherwise have
//     Keeper read it back and render it into that host's own params — a read
//     primitive over the whole KV store, handed to the attacker. The keeper bucket
//     is produced in-process by keeper-side core modules and has no such path.
//
//  2. Only the service's OWN namespace: `<mount>/<service>/<incarnation>/…`. That
//     is exactly what a service may address without declaring anything ([ADR-0083]
//     §1 — the derived path). A reference pointing anywhere else is left as the
//     literal string it is, NOT resolved and NOT an error: a keeper module may
//     legitimately carry a `vault:` string as data, and refusing it here would turn
//     an unrelated value into a failed run.
//
// The mount is not checked — render does not know keeper.yml's `vault.kv_mount`,
// and it does not need to: what makes the path the service's own is the
// (service, incarnation) pair, not which mount they live on.
// The second return value is the set of register names in which at least one
// reference actually resolved — the seal source for `${ register.<name>.… }`
// (a `register.<name>` address in [cel.SealSources.Fields]). It is built from what resolved rather than
// from the schema so that a register carrying no secret is not sealed, and a cell
// reading a register that DOES carry one is sealed even when the author reached
// for a neighbouring field.
func (p *Pipeline) resolveRegisterSecrets(ctx context.Context, in RenderInput) (map[string]any, map[string]bool, error) {
	if len(in.KeeperRegister) == 0 {
		return in.KeeperRegister, nil, nil
	}
	service, incarnation := in.Incarnation.Service, in.Incarnation.ID
	if service == "" || incarnation == "" {
		// No owner to check a namespace against — resolve nothing rather than
		// resolve everything (push/trial/unit callers).
		return in.KeeperRegister, nil, nil
	}
	out := make(map[string]any, len(in.KeeperRegister))
	var sealed map[string]bool
	for name, payload := range in.KeeperRegister {
		n := 0
		rv, err := walkRegisterValue(ctx, p.vault, payload, service, incarnation, &n)
		if err != nil {
			return nil, nil, err
		}
		out[name] = rv
		if n > 0 {
			if sealed == nil {
				sealed = map[string]bool{}
			}
			sealed[name] = true
		}
	}
	return out, sealed, nil
}

// walkRegisterValue mirrors [walkVaultValue], with the own-namespace gate on each
// candidate string.
// resolved counts the references this walk actually read, so the caller can seal
// exactly the registers that carry a secret.
func walkRegisterValue(ctx context.Context, vc KVReader, v any, service, incarnation string, resolved *int) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := walkRegisterValue(ctx, vc, val, service, incarnation, resolved)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			rv, err := walkRegisterValue(ctx, vc, val, service, incarnation, resolved)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	case string:
		if !ownNamespaceRef(t, service, incarnation) {
			return t, nil
		}
		rv, err := readVaultRef(ctx, vc, t)
		if err != nil {
			return nil, err
		}
		*resolved++
		return rv, nil
	default:
		return v, nil
	}
}

// WithVaultFence returns ctx carrying the two things every CEL pass that can call
// vault() needs: the per-pass resolution memo and the §7 own-namespace guard.
//
// They are bound together because they are not independent. The guard runs inside
// vault() BEFORE the memoized read, so a ctx carrying the memo but not the guard
// caches a value the fence would have refused, and the next caller in the pass is
// served that value without the guard ever being consulted. One constructor makes
// the pair impossible to split by accident.
//
// The memo is per-PASS, not per-run: Render and EvalAsserts each
// take their own. `core.state.*` mints a secret between the render pass and
// the state-op pass, and a read carried across that boundary would serve the
// pre-mint value.
//
// A nil ctx is accepted (context.Background()); an empty service fences nothing,
// the condition ownNamespaceVaultGuard already applies.
func WithVaultFence(ctx context.Context, service string) context.Context {
	ctx = cel.WithVaultMemo(ctx)
	return cel.WithVaultPathGuard(ctx, ownNamespaceVaultGuard(service))
}

// ownNamespaceVaultGuard is the runtime half of the §7 fence ([ADR-0083]), and the
// half that catches what static text cannot: `vault(vars.p)` names no segment
// config.ScanOwnNamespaceVault could compare, because the path does not exist until
// CEL has evaluated the argument. The guard runs on the evaluated path, so how it was
// assembled stops mattering.
//
// Scoped to the whole prefix rather than to this incarnation, deliberately and
// symmetrically with the load-time scan: the platform owns `<mount>/<service>/…` for
// every incarnation, so an author reaching into a SIBLING incarnation's namespace is
// the same second-copy failure, not a lesser one.
//
// An empty service (push/trial, no incarnation identity) fences nothing — the same
// condition resolveRegisterSecrets applies. The platform's own reads are unaffected:
// a declared secret is resolved through readVaultRef, never through this macro.
func ownNamespaceVaultGuard(service string) cel.VaultPathGuard {
	if service == "" {
		return nil
	}
	return func(path string) error {
		if !config.PathAddressesOwnNamespace(path, service) {
			return nil
		}
		return fmt.Errorf("%s: %s is under the namespace the platform derives %q's own secrets into; declare the field as `type: secret` in state_schema instead of naming a path ([ADR-0083] §7)",
			config.VaultOwnNamespaceCode, path, service)
	}
}

// ownNamespaceRef reports whether ref is a `vault:` reference into
// `<mount>/<service>/<incarnation>/…` — the namespace a service's own declared
// secrets derive into.
//
// The segments are compared whole. A prefix comparison would accept
// `secret/<mount>/wb-service-redis-evil/…`, and a `..` segment is rejected outright
// rather than cleaned: this is a gate, and a gate that normalises its input decides
// on a path different from the one it was given.
func ownNamespaceRef(ref, service, incarnation string) bool {
	if !strings.HasPrefix(ref, vaultRefPrefix) {
		return false
	}
	body := strings.TrimPrefix(ref, vaultRefPrefix)
	if i := strings.LastIndexByte(body, '#'); i >= 0 {
		body = body[:i]
	}
	segs := strings.Split(body, "/")
	if len(segs) < 4 {
		// mount + service + incarnation + at least the state field.
		return false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return segs[1] == service && segs[2] == incarnation
}
