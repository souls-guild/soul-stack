package cloud

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"

	"google.golang.org/protobuf/types/known/structpb"
)

// Params that select the CloudDriver and the credentials it is called with
// (NIM-668). Two sources, never both:
//
//   - registry — `provider: <name>` resolves an entry of the `providers`
//     registry into driver + credentials + region + fqdn_suffix (Variant A,
//     ADR-017);
//   - inline — `driver` + `credentials` (+ optional `region` / `fqdn_suffix`)
//     carry the same tuple in the step itself, so a fleet can provision with
//     ZERO rows in the registries.
//
// Both sources converge on one *ResolvedProvider before anything is called, so
// the driver, the audit event and the output cannot tell them apart.
const (
	paramProvider    = "provider"
	paramDriver      = "driver"
	paramCredentials = "credentials"
	paramRegion      = "region"
	paramFQDNSuffix  = "fqdn_suffix"
	paramProfile     = "profile"
)

// providerParams is the parsed, source-agnostic view of those params. Absence is
// the empty string for the scalars; `credentials` carries its own presence flag
// because an empty object is still an answer ("this driver takes none") and must
// not read as "not set".
type providerParams struct {
	provider       string
	driver         string
	credentials    map[string]any
	credentialsSet bool
	region         string
	fqdnSuffix     string
	// driverBroken / sourceBroken record a source param the author DID write
	// and got the type of. Its own error already names it; the source rules
	// must then stay quiet, or the step is told both "driver is not a string"
	// and "no cloud provider source" — the second sends the author looking for
	// a param they are already holding.
	driverBroken bool
	sourceBroken bool
}

// inline reports whether the step tried to carry the driver itself. One half is
// enough to count: a step naming a driver without credentials has chosen the
// inline source and got it wrong, and must be told that — not told that no
// source was set at all.
func (p providerParams) inline() bool { return p.driver != "" || p.credentialsSet }

// providerSource is a resolved CloudDriver tuple plus how it was selected.
type providerSource struct {
	resolved *ResolvedProvider
	// label is what the audit event and the error messages name as the
	// provider: the registry entry name in registry mode, the driver alias
	// inline — there is no registry name there to print.
	label string
	// inline records the source, so the error for a missing fqdn_suffix can
	// point at the step param or at the registry column, whichever the author
	// is actually holding.
	inline bool
}

// parseProviderParams reads the source params for the phase the caller is in.
// Type errors are collected rather than returned one at a time, so a step with
// two mistakes reports both in one pass — the same shape [Module.Validate] uses.
//
// The phase reaches only `credentials`; every other param has one shape on both
// sides of vault-resolve, which is why the source RULES stay shared.
func parseProviderParams(params *structpb.Struct, phase credentialsPhase) (providerParams, []string) {
	var pp providerParams
	var errs []string

	for _, f := range []struct {
		name   string
		dst    *string
		broken *bool
	}{
		{paramProvider, &pp.provider, &pp.sourceBroken},
		{paramDriver, &pp.driver, &pp.driverBroken},
		{paramRegion, &pp.region, nil},
		{paramFQDNSuffix, &pp.fqdnSuffix, nil},
	} {
		v, err := util.OptStringParam(params, f.name)
		if err != nil {
			errs = append(errs, err.Error())
			if f.broken != nil {
				*f.broken = true
			}
			continue
		}
		*f.dst = v
	}
	pp.sourceBroken = pp.sourceBroken || pp.driverBroken

	// set, not `creds != nil`: authored, the correct form is a `vault:` ref that
	// carries no map yet, and an unusable value was still WRITTEN — saying "no
	// cloud provider source" on top of either would send the author looking for
	// a param they already set.
	creds, set, err := credentialsParam(params, phase)
	if err != nil {
		errs = append(errs, err.Error())
	}
	pp.credentials, pp.credentialsSet = creds, set
	return pp, errs
}

// vaultRefPrefix is what marks a cell the vault-resolve phase will replace.
// Local by the repo's convention — every consumer keeps its own copy
// (shared/audit/mask.go, shared/config/input_value.go, keeper/internal/render);
// the authority is render.vaultRefPrefix, which decides what actually resolves.
const vaultRefPrefix = "vault:"

// exprMarker opens a CEL interpolation (ADR-010). Authored, it is the shape of
// the one mistake this param invites: an expression the vault phase runs too
// early to see.
const exprMarker = "${"

// credentialsPhase names which side of the vault-resolve boundary a caller is
// on. It exists because the cell has a DIFFERENT legitimate shape on each side,
// so one reader cannot judge both: before the phase it is the `vault:` ref the
// author wrote, after it the secret MAP that phase put there (ADR-010, phase 1).
// Reading both with one predicate is what made [Module.Validate] refuse the only
// correct authored form while [Module.Apply] accepted it.
type credentialsPhase int

const (
	// authoredParams is what Validate reads: the definition as written, before
	// anything resolved.
	authoredParams credentialsPhase = iota
	// resolvedParams is what Apply reads: after the vault phase replaced the ref.
	resolvedParams
)

// credentialsParam reads `credentials` for the phase the caller is in and
// reports whether the param was SET as well as what it holds — presence and
// value part company here, because the authored form is a string that carries no
// map yet and must still count as "this step chose the inline source".
//
// What each phase accepts:
//
//   - authored: the `vault:` ref (correct — the map arrives later); any other
//     string is a credential value written into a definition, and refused.
//   - resolved: the secret map (correct); a `vault:` ref that survived means
//     nothing resolved it — near-always because it was built by an expression,
//     which the vault phase runs too early to see (ADR-010) — and any other
//     string is again a literal value.
//
// No error echoes the value it read.
func credentialsParam(params *structpb.Struct, phase credentialsPhase) (creds map[string]any, set bool, err error) {
	v, ok := params.GetFields()[paramCredentials]
	if !ok || v == nil {
		return nil, false, nil
	}
	switch k := v.Kind.(type) {
	case *structpb.Value_NullValue:
		return nil, false, nil
	case *structpb.Value_StructValue:
		return k.StructValue.AsMap(), true, nil
	case *structpb.Value_StringValue:
		s := k.StringValue
		switch {
		case phase == authoredParams && strings.HasPrefix(s, vaultRefPrefix):
			return nil, true, nil
		case strings.HasPrefix(s, vaultRefPrefix), strings.Contains(s, exprMarker):
			// One root cause, whichever side saw it: a ref an expression builds
			// is invisible to the phase that resolves refs, so it arrives at the
			// driver as the ref itself.
			return nil, true, fmt.Errorf("param %q: the reference was never resolved -- write it literally, not through an expression (keeper reads vault BEFORE it renders `${ … }`, so a ref an expression produces arrives too late)", paramCredentials)
		}
		return nil, true, fmt.Errorf("param %q: expected a vault reference, got a plain string -- write `%s: vault:<mount>/<path>` (a credential value must never be written into a definition)", paramCredentials, paramCredentials)
	default:
		return nil, true, fmt.Errorf("param %q: expected an object, got %T", paramCredentials, v.Kind)
	}
}

// checkProviderParams enforces the source rules. It is the ONLY place that
// decides them: [Module.Apply] calls it through [Module.resolveProviderSource]
// (the enforcement that actually runs — a keeper-side core module is dispatched
// straight to Apply, Validate is never constructed for it in production) and
// [Module.Validate] calls it directly, so the two cannot disagree about WHICH
// SOURCE a step chose. They can still differ about the SHAPE of `credentials`,
// and must: they read the step on opposite sides of vault-resolve, so that one
// judgement is the caller's phase to make (see [credentialsPhase]).
//
// Two rules:
//
//   - exactly one source. `provider` and the inline pair are mutually exclusive,
//     and one of them is required.
//   - `region` / `fqdn_suffix` belong to the inline source only. In registry
//     mode they name columns of the registry entry, and a step quietly
//     overriding them would move a fleet to another region on a param the
//     author read as documentation. Refused, not preferred.
//
// `profile` is NOT part of this: the `profiles` registry is independent of the
// `providers` one, so an inline driver may still name a registry profile and a
// registry provider may still take an inline spec (see [Module.resolveProfileParam]).
func checkProviderParams(pp providerParams) []string {
	var errs []string
	switch {
	case pp.provider != "" && pp.inline():
		errs = append(errs, fmt.Sprintf("params %q and %q/%q are mutually exclusive: %q names an entry of the providers registry, the inline pair carries the driver and its credentials in the step -- set one source, not both",
			paramProvider, paramDriver, paramCredentials, paramProvider))
	case pp.provider == "" && !pp.inline() && !pp.sourceBroken:
		errs = append(errs, fmt.Sprintf("no cloud provider source: set %q (an entry of the providers registry) or the inline pair %q + %q",
			paramProvider, paramDriver, paramCredentials))
	case pp.inline() && pp.driver == "" && !pp.driverBroken:
		errs = append(errs, fmt.Sprintf("param %q requires %q: the plugin alias to call (for example `%s: wb`)",
			paramCredentials, paramDriver, paramDriver))
	case pp.inline() && !pp.credentialsSet:
		// `vault:<mount>/<path>`, never a concrete ref: a real-looking one is
		// what the masker hunts by content, and it would replace this whole
		// sentence with ***MASKED*** in error_summary — the author would be told
		// nothing at the moment they need telling.
		errs = append(errs, fmt.Sprintf("param %q requires %q: a vault reference keeper reads for you (for example `%s: vault:<mount>/<path>`)",
			paramDriver, paramCredentials, paramCredentials))
	}
	if pp.provider != "" {
		for _, f := range []struct{ name, value, column string }{
			{paramRegion, pp.region, "providers.region"},
			{paramFQDNSuffix, pp.fqdnSuffix, "providers.fqdn_suffix"},
		} {
			if f.value == "" {
				continue
			}
			errs = append(errs, fmt.Sprintf("params %q and %q are mutually exclusive: the registry entry already holds %s -- change it there, or drop %q and pass the driver inline",
				paramProvider, f.name, f.column, paramProvider))
		}
	}
	return errs
}

// checkSourceParams answers the source rules from the params alone, with no I/O.
// [Module.Apply] calls it first thing, because the registry path used to refuse a
// missing `provider` before it did anything external and must keep doing so: the
// resolve that replaced that check sits behind a userdata render, which reads the
// CA from Vault. A step that named no source, or two, cannot succeed — it should
// cost nothing to find out.
func checkSourceParams(params *structpb.Struct) error {
	pp, errs := parseProviderParams(params, resolvedParams)
	errs = append(errs, checkProviderParams(pp)...)
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// resolveProviderSource turns the source params into the tuple the driver is
// called with. Registry mode resolves the entry; inline mode builds the same
// struct from the step. Every failure is one string, ready for a failed-event.
func (m *Module) resolveProviderSource(ctx context.Context, params *structpb.Struct, state string) (providerSource, error) {
	pp, errs := parseProviderParams(params, resolvedParams)
	errs = append(errs, checkProviderParams(pp)...)
	if len(errs) > 0 {
		return providerSource{}, errors.New(strings.Join(errs, "; "))
	}

	if pp.provider == "" {
		// The registry writes its `region` column into the credentials map under
		// the same key (see credentials.go), so a driver reads region from one
		// place whichever source produced it. The inline write happens only when
		// the param is set: unlike a registry entry, which always has the column,
		// an absent param here must leave a `region` the secret itself carries
		// alone.
		creds := make(map[string]any, len(pp.credentials)+1)
		for k, v := range pp.credentials {
			creds[k] = v
		}
		if pp.region != "" {
			creds[regionKey] = pp.region
		}
		return providerSource{
			resolved: &ResolvedProvider{Driver: pp.driver, Credentials: creds, FQDNSuffix: pp.fqdnSuffix},
			label:    pp.driver,
			inline:   true,
		}, nil
	}

	// A-flow: keeper resolves the Provider registry to driver-name +
	// plain-credentials (+ FQDNSuffix for self-onboard prediction); the driver
	// never touches Vault.
	if m.Resolver == nil {
		return providerSource{}, fmt.Errorf("cloud %s: provider resolver not configured (wire CredentialsResolverPG in main)", state)
	}
	resolved, err := m.Resolver.Resolve(ctx, pp.provider)
	if err != nil {
		return providerSource{}, fmt.Errorf("resolve provider %q: %s", pp.provider, maskErr(err))
	}
	return providerSource{resolved: resolved, label: pp.provider}, nil
}

// profileValue splits `profile` into its two accepted forms without touching the
// registry: a string is the NAME of an entry of the `profiles` registry, an
// object is the VM spec itself, handed to the driver as-is. Absent/null is
// neither — a VM with no spec.
//
// The type is the discriminator because the two forms cannot collide: a registry
// name is never an object and a spec is never a bare string.
func profileValue(params *structpb.Struct) (name string, spec map[string]any, err error) {
	v, ok := params.GetFields()[paramProfile]
	if !ok || v == nil {
		return "", nil, nil
	}
	switch k := v.Kind.(type) {
	case *structpb.Value_NullValue:
		return "", nil, nil
	case *structpb.Value_StructValue:
		return "", k.StructValue.AsMap(), nil
	case *structpb.Value_StringValue:
		return k.StringValue, nil, nil
	default:
		return "", nil, fmt.Errorf("param %q: expected an object (the VM spec itself) or a string (the name of an entry of the profiles registry), got %T", paramProfile, v.Kind)
	}
}

// resolveProfileParam resolves `profile` into the VM-spec map the driver is
// handed. An inline spec is passed through untouched; a name is looked up in the
// `profiles` registry, which is independent of the `providers` one — naming a
// profile alongside an inline driver is allowed, and is the point of the split
// (a fleet keeps sizes in the registry while the driver comes from the step).
func (m *Module) resolveProfileParam(ctx context.Context, params *structpb.Struct) (map[string]any, error) {
	name, spec, err := profileValue(params)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return spec, nil
	}
	if m.Resolver == nil {
		return nil, fmt.Errorf("param %q: %q is the name of an entry of the profiles registry, which is not configured here -- pass the VM spec inline as an object instead", paramProfile, name)
	}
	resolved, err := m.Resolver.ResolveProfile(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("resolve profile %q: %s", name, maskErr(err))
	}
	return resolved, nil
}
