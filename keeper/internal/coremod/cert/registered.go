// Package cert implements the keeper-side core module `core.cert.registered`
// (cert-rotation Var1, E1) — registers a SERVICE TLS certificate of an
// incarnation in the Warrant registry (keeper/internal/cert) so that the Reaper
// rule `rotate_due_certs` can see its expiration (`not_after`) and rotate it
// centrally.
//
// State:
//   - registered: declarative form "certificate(s) of incarnation are registered
//     in Warrant by their expiration". Without this step, Reaper is blind to
//     PRIMARY certificates (issued by operator or in create-scenario) —
//     registration must be part of create/rotate_tls scenario.
//
// Mechanics: for each cert in `certs[]`, the module READS PEM from Vault via
// `vault_ref` (form `<mount>/<path>#<field>`), parses x509 and extracts
// serial_number / fingerprint / not_after FROM THE CERTIFICATE ITSELF (autonomously
// — does not require the scenario to fetch PKI metadata). Then registers an
// active-row in Warrant (supersede previous active of the same kind + insert new
// in a single tx).
//
// IDEMPOTENCE: if an active-row with the same fingerprint already exists — no-op
// (changed=false). New fingerprint (certificate changed) → supersede+insert,
// changed=true.
//
// SECURITY (ADR-010): output/audit only contains NON-secret metadata (kind +
// fingerprint + serial + not_after); PEM itself / private key do not leave
// (module reads only the PUBLIC certificate, does not touch the private key).
package cert

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/certissue"
	"github.com/souls-guild/soul-stack/keeper/internal/certpolicy"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name is the base name of the module without state suffix (Registry key). The
// author form of the task address is `core.cert.registered` (base + state, like
// keeper-side core modules); state `registered` arrives in pluginv1.ApplyRequest.state.
const Name = "core.cert"

// StateRegistered — registration of an already-existing cert in the Warrant registry.
const StateRegistered = "registered"

// StateIssued — Keeper ITSELF issues the cert: keypair+CSR → sign with the Vault PKI
// role from the manifest → write cert/key to Vault → register in Warrant (NIM-99 Slice C).
const StateIssued = "issued"

// VaultReader — narrow surface of vault.Client that the module needs: reading a
// KV path to extract the cert's PEM. Narrowing simplifies unit tests (fake without HTTP).
type VaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// Store is a narrow transactional surface of the Warrant registry: registration
// of active-row (supersede+insert in tx) + reading current active (idempotency
// check). Production: [PGStore] over *pgxpool.Pool.
type Store interface {
	SelectActive(ctx context.Context, incarnationID string, kind keepercert.Kind) (*keepercert.Warrant, error)
	RegisterActive(ctx context.Context, w *keepercert.Warrant) error
}

// AuditWriter is a narrow dependency for the `cert.registered` audit-event.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// IssuePolicyResolver resolves the incarnation's cert-rotation policy from its
// manifest (certpolicy.Resolver satisfies it). For state `issued` the PKI-signing
// role and service name are taken FROM HERE, not from params.
type IssuePolicyResolver interface {
	Resolve(ctx context.Context, incarnationName string) (certpolicy.Policy, error)
}

// Module — implementation of sdk/module.SoulModule. One module for base-name `core.cert`.
type Module struct {
	Vault VaultReader
	Store Store
	Audit AuditWriter
	// KID is the Keeper instance identifier, written to warrant.issued_by_kid
	// (audit: "which instance registered the cert"). Empty → NULL.
	KID string

	// Dependencies for state `issued` (NIM-99 Slice C). Set by the wire-up slice
	// in registry.go AFTER New; nil for any of them → issued returns failed (not
	// configured). state `registered` does not depend on them.
	Signer      certissue.Signer     // PKI signing of the CSR
	VaultWriter certissue.KVWriter   // writing cert/key to Vault
	Policy      IssuePolicyResolver  // rotation policy resolver from the manifest
	CSRGen      certissue.CSRGenFunc // keypair+CSR generation (keeper-side, R2)
	PKIMount    func() string        // hot-reload keeper.yml Vault.PKIMount
}

// New — wire-helper. issued-dependencies (Signer/VaultWriter/Policy/CSRGen/
// PKIMount) are set separately after the constructor.
func New(v VaultReader, s Store, a AuditWriter, kid string) *Module {
	return &Module{Vault: v, Store: s, Audit: a, KID: kid}
}

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case StateRegistered:
		if _, err := util.StringParam(req.Params, "incarnation"); err != nil {
			errs = append(errs, err.Error())
		}
		if _, err := parseCertTargets(req); err != nil {
			errs = append(errs, err.Error())
		}
	case StateIssued:
		if _, err := util.StringParam(req.Params, "incarnation"); err != nil {
			errs = append(errs, err.Error())
		}
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (want %s or %s)", req.State, StateRegistered, StateIssued))
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// certTarget is one registration target: kind + Vault-ref to PEM.
type certTarget struct {
	kind     keepercert.Kind
	vaultRef string
}

// parseCertTargets parses `params.certs` — a non-empty list of
// `{kind: cert|key|ca, vault_ref: <str>}`. Empty/absent is an error.
func parseCertTargets(req *pluginv1.ValidateRequest) ([]certTarget, error) {
	return parseCertTargetsFromStruct(req.Params)
}

// parseCertTargetsFromStruct is common parsing of `certs[]` for Validate/Apply
// (their Request types have different wrappers but identical .Params field →
// *structpb.Struct).
func parseCertTargetsFromStruct(params *structpb.Struct) ([]certTarget, error) {
	raw, err := util.ListParam(params, "certs")
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("param \"certs\": empty list")
	}
	out := make([]certTarget, 0, len(raw))
	for i, item := range raw {
		sv := item.GetStructValue()
		if sv == nil {
			return nil, fmt.Errorf("param \"certs\"[%d]: expected object", i)
		}
		kindStr, kerr := util.StringParam(sv, "kind")
		if kerr != nil {
			return nil, fmt.Errorf("param \"certs\"[%d].%v", i, kerr)
		}
		kind := keepercert.Kind(kindStr)
		if kind != keepercert.KindCert && kind != keepercert.KindKey && kind != keepercert.KindCA {
			return nil, fmt.Errorf("param \"certs\"[%d].kind: invalid %q (want cert/key/ca)", i, kindStr)
		}
		ref, rerr := util.StringParam(sv, "vault_ref")
		if rerr != nil {
			return nil, fmt.Errorf("param \"certs\"[%d].%v", i, rerr)
		}
		if ref == "" {
			return nil, fmt.Errorf("param \"certs\"[%d].vault_ref: empty", i)
		}
		out = append(out, certTarget{kind: kind, vaultRef: ref})
	}
	return out, nil
}

// Apply dispatches on req.State: `registered` — registration of an already
// existing cert; `issued` — Keeper issues the cert itself (applyIssued in
// issued.go).
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	switch req.State {
	case StateRegistered:
		return m.applyRegistered(req, stream)
	case StateIssued:
		return applyIssued(m, req, stream)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q (want %s or %s)", req.State, StateRegistered, StateIssued))
	}
}

// applyRegistered reads the PEM of each cert from Vault, extracts metadata, and
// registers the active Warrant row (idempotent by fingerprint). changed=true
// if at least one cert was written (new / changed fingerprint).
func (m *Module) applyRegistered(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	incarnation, err := util.StringParam(req.Params, "incarnation")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	targets, err := parseCertTargetsApply(req)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// Optional PKI metadata (for audit of cert origin; not part of rotation
	// predicate). auto_rotate defaults to true.
	pkiMount, err := util.OptStringParam(req.Params, "pki_mount")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	pkiRole, err := util.OptStringParam(req.Params, "pki_role")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	autoRotate, hasAuto, err := util.OptBoolParam(req.Params, "auto_rotate")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !hasAuto {
		autoRotate = true
	}

	var registered []map[string]any // non-secret metadata of written certs
	anyChanged := false

	for _, t := range targets {
		meta, rerr := m.readCertMeta(ctx, t.vaultRef)
		if rerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("cert %s (%s): %v", t.kind, t.vaultRef, rerr))
		}

		// Idempotency: active-row with the same fingerprint → no-op.
		cur, cerr := m.Store.SelectActive(ctx, incarnation, t.kind)
		if cerr != nil && !errors.Is(cerr, keepercert.ErrNotFound) {
			return util.SendFailed(stream, fmt.Sprintf("cert %s: read active: %v", t.kind, cerr))
		}
		if cur != nil && cur.Fingerprint == meta.fingerprint {
			continue // same cert already registered
		}

		w := &keepercert.Warrant{
			IncarnationID: incarnation,
			Kind:          t.kind,
			VaultRef:      t.vaultRef,
			SerialNumber:  meta.serial,
			Fingerprint:   meta.fingerprint,
			NotAfter:      meta.notAfter,
			Status:        keepercert.StatusActive,
			AutoRotate:    autoRotate,
		}
		if pkiMount != "" {
			w.PKIMount = &pkiMount
		}
		if pkiRole != "" {
			w.PKIRole = &pkiRole
		}
		if m.KID != "" {
			w.IssuedByKID = &m.KID
		}

		if regErr := m.Store.RegisterActive(ctx, w); regErr != nil {
			return util.SendFailed(stream, fmt.Sprintf("cert %s: register: %v", t.kind, regErr))
		}
		anyChanged = true
		registered = append(registered, map[string]any{
			"kind":          string(t.kind),
			"fingerprint":   meta.fingerprint,
			"serial_number": meta.serial,
			"not_after":     meta.notAfter.UTC().Format(time.RFC3339),
		})
	}

	if m.Audit != nil && anyChanged {
		ev := &audit.Event{
			EventType:     audit.EventCertRegistered,
			Source:        audit.SourceKeeperInternal,
			CorrelationID: incarnation,
			Payload: map[string]any{
				"incarnation": incarnation,
				"certs":       registered, // non-secret metadata only
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	// Deterministic output: kinds of written certs (sorted), no secrets.
	kinds := make([]string, 0, len(registered))
	for _, r := range registered {
		kinds = append(kinds, r["kind"].(string))
	}
	sort.Strings(kinds)
	kindsAny := make([]any, len(kinds))
	for i, k := range kinds {
		kindsAny[i] = k
	}
	out := map[string]any{
		"registered_kinds": kindsAny,
	}
	return util.SendFinal(stream, anyChanged, out)
}

// parseCertTargetsApply is the same parsing as parseCertTargets, but for
// ApplyRequest (Validate/Apply-Request have different types, both carry .Params).
func parseCertTargetsApply(req *pluginv1.ApplyRequest) ([]certTarget, error) {
	return parseCertTargetsFromStruct(req.Params)
}

// certMeta contains metadata extracted from PEM certificate.
type certMeta struct {
	serial      string
	fingerprint string
	notAfter    time.Time
}

// readCertMeta reads PEM from Vault via ref (`<mount>/<path>#<field>`), parses
// the first CERTIFICATE block and extracts serial/fingerprint/not_after.
//
// kind=key also references a ref with a certificate (a private key has no own
// not_after — it lives together with cert): the author puts in certs[key].vault_ref
// the same server cert as in certs[cert] (material parity) so that the warrant-row
// for key carries the correct expiration. This is valid: the registry stores the
// EXPIRATION, and vault_ref key indicates what to rotate (private key).
func (m *Module) readCertMeta(ctx context.Context, ref string) (certMeta, error) {
	path, field, err := splitVaultRef(ref)
	if err != nil {
		return certMeta{}, err
	}
	data, err := m.Vault.ReadKV(ctx, path)
	if err != nil {
		return certMeta{}, fmt.Errorf("vault read %q: %w", path, err)
	}
	pemStr, err := selectPEMField(data, field)
	if err != nil {
		return certMeta{}, err
	}
	c, err := parseFirstCertificate([]byte(pemStr))
	if err != nil {
		return certMeta{}, err
	}
	return certMeta{
		serial:      c.SerialNumber.Text(16),
		fingerprint: keepercert.FingerprintFromCert(c),
		notAfter:    c.NotAfter.UTC(),
	}, nil
}

// splitVaultRef splits `<mount>/<path>#<field>` into logical-path and field.
// logical-path goes to ReadKV as-is (without `vault:` prefix) — symmetry with CEL
// `vault('secret/...#field')` (shared/cel splitVaultField): author refs for service
// certs are pure logical-path, like essence tls_cert_ref "secret/services/redis/tls#cert".
// ReadKV normalizes the path itself (relativeKVPath: strip mount + fail-closed
// guard on `..`).
func splitVaultRef(ref string) (path, field string, err error) {
	body := ref
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		body, field = ref[:i], ref[i+1:]
		if field == "" {
			return "", "", fmt.Errorf("vault-ref %q: empty field after '#'", ref)
		}
	}
	if body == "" {
		return "", "", fmt.Errorf("vault-ref %q: empty path", ref)
	}
	return body, field, nil
}

// selectPEMField extracts a string field from the secret. Without field and
// exactly one field — takes it; otherwise field is required (multiple fields —
// ambiguous).
func selectPEMField(data map[string]any, field string) (string, error) {
	if field == "" {
		if len(data) != 1 {
			return "", fmt.Errorf("vault-ref without #field but secret has %d fields (specify #field)", len(data))
		}
		for _, v := range data {
			s, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("vault secret field is not a string")
			}
			return s, nil
		}
	}
	v, ok := data[field]
	if !ok {
		return "", fmt.Errorf("field %q absent in secret", field)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("field %q is not a string", field)
	}
	return s, nil
}

// parseFirstCertificate finds the first CERTIFICATE PEM block and parses it
// into x509. The leaf cert is placed first in the PEM chain — we take it
// (its not_after is the expiration we use for rotation).
func parseFirstCertificate(pemBytes []byte) (*x509.Certificate, error) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, fmt.Errorf("no CERTIFICATE block in PEM")
		}
		if block.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse certificate: %w", err)
			}
			return c, nil
		}
	}
}
