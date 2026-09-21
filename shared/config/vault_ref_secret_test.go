package config

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// NIM-505 guard: a value rejected for not being a vault-ref must not reach any
// surface that outlives the operator's terminal — above all `audit_log`, which is
// append-only and pruned only by the 365-day retention job. A `*_ref` field is
// where an operator pastes a password by mistake; that is exactly the value the
// diagnostic used to quote back with `%q`.
//
// The canary is a plausible plaintext password and deliberately NOT a vault-ref,
// so every one of the thirteen fields rejects it.
const vaultRefCanary = "nim505-canary-Pv73hQz-do-not-log"

// vaultRefCanaryConfig is a keeper.yml that puts the canary in EVERY field either
// vault-ref validator guards — nine in the semantic phase ([checkVaultRef]) and
// four in the schema phase ([isVaultRef]). Both phases run in the same pass
// (parseAndValidate steps 4 and 5), so one document exercises both.
//
// The blocks around the refs are otherwise valid: the point is to be rejected for
// the ref, not to be rejected so early that the ref check never runs.
const vaultRefCanaryConfig = `kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: ` + vaultRefCanary + `
  pool: { min: 1, max: 5 }
redis:
  mode: sentinel
  master_name: mymaster
  sentinels: ["s1:26379", "s2:26379", "s3:26379"]
  password_ref: ` + vaultRefCanary + `
  sentinel_password_ref: ` + vaultRefCanary + `
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
cloud_init:
  tls_ca_ref: ` + vaultRefCanary + `
auth:
  jwt:
    signing_key_ref: ` + vaultRefCanary + `
  ldap:
    url: "ldaps://ldap.example.com:636"
    bind_mode: search
    bind_dn: "cn=svc,dc=example,dc=com"
    bind_password_ref: ` + vaultRefCanary + `
    base_dn: "ou=people,dc=example,dc=com"
    user_filter: "(uid=%s)"
    tls:
      ca_ref: ` + vaultRefCanary + `
  oidc:
    issuer: "https://idp.example.com/realms/soul"
    client_id: soul-keeper
    client_secret_ref: ` + vaultRefCanary + `
    redirect_url: "https://keeper.example.com/auth/oidc/callback"
    tls:
      ca_ref: ` + vaultRefCanary + `
metrics:
  auth:
    basic:
      enabled: true
      username: prom
      password_ref: ` + vaultRefCanary + `
push:
  host_ca_ref: ` + vaultRefCanary + `
  host_ca_refs:
    - name: prod
      ref: ` + vaultRefCanary + `
sigil:
  signing_key_ref: ` + vaultRefCanary + `
`

// vaultRefCanaryPaths — every yaml-path the config above must be rejected at,
// with the code its validator uses. Written out rather than counted: a floor of
// "at least one" would let twelve fields go silently unchecked, and the two codes
// are what tells the two validators apart.
var vaultRefCanaryPaths = map[string]string{
	// semantic-validate (checkVaultRef)
	"$.postgres.dsn_ref":            "vault_ref_invalid_format",
	"$.redis.password_ref":          "vault_ref_invalid_format",
	"$.redis.sentinel_password_ref": "vault_ref_invalid_format",
	"$.auth.jwt.signing_key_ref":    "vault_ref_invalid_format",
	"$.cloud_init.tls_ca_ref":       "vault_ref_invalid_format",
	"$.auth.ldap.bind_password_ref": "vault_ref_invalid_format",
	"$.auth.ldap.tls.ca_ref":        "vault_ref_invalid_format",
	"$.auth.oidc.client_secret_ref": "vault_ref_invalid_format",
	"$.auth.oidc.tls.ca_ref":        "vault_ref_invalid_format",
	// schema-validate (isVaultRef)
	"$.metrics.auth.basic.password_ref": "vault_ref_invalid",
	"$.push.host_ca_ref":                "vault_ref_invalid",
	"$.push.host_ca_refs[0].ref":        "vault_ref_invalid",
	"$.sigil.signing_key_ref":           "vault_ref_invalid",
}

// assertVaultRefCoverage is the non-vacuity floor: every field in
// vaultRefCanaryPaths must have produced its diagnostic. Without it a change that
// stops validating a ref — or a fixture that stops reaching one of the blocks —
// would make the canary-absent assertions below pass for the wrong reason.
//
// It also demands the mask token in each message: "the value is gone" is not
// enough, because dropping the field entirely would hide from the operator that a
// value was rejected at all. The operator must be able to see WHICH field, and
// that something was there.
func assertVaultRefCoverage(t *testing.T, diags []diag.Diagnostic) {
	t.Helper()
	seen := map[string]bool{}
	for _, d := range diags {
		want, ok := vaultRefCanaryPaths[d.YAMLPath]
		if !ok {
			continue
		}
		if d.Code != want {
			t.Errorf("%s: code = %q, want %q", d.YAMLPath, d.Code, want)
		}
		if !strings.Contains(d.Message, audit.MaskedValue) {
			t.Errorf("%s: message %q carries no %s — the field was dropped instead of masked, "+
				"so the operator cannot tell a value was rejected", d.YAMLPath, d.Message, audit.MaskedValue)
		}
		if !strings.Contains(d.Message, fieldFromYAMLPath(d.YAMLPath)) {
			t.Errorf("%s: message %q does not name the field — masking must not cost the operator "+
				"the one thing they need to fix it", d.YAMLPath, d.Message)
		}
		seen[d.YAMLPath] = true
	}
	var missing []string
	for p := range vaultRefCanaryPaths {
		if !seen[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		dump(t, diags)
		t.Fatalf("no vault-ref diagnostic for %d of %d fields: %v — the fixture no longer reaches them, "+
			"so the leak assertions below prove nothing about those fields",
			len(missing), len(vaultRefCanaryPaths), missing)
	}
}

// assertCanaryAbsent checks one rendered surface.
func assertCanaryAbsent(t *testing.T, surface, blob string) {
	t.Helper()
	if strings.Contains(blob, vaultRefCanary) {
		t.Errorf("%s carries the rejected value %q in clear text:\n%s", surface, vaultRefCanary, blob)
	}
}

// TestVaultRef_RejectedValueNeverReachesAuditLog — NIM-505 GUARD.
//
// Drives the real hot-reload path (a good config is loaded, then replaced on disk
// with vaultRefCanaryConfig and reloaded) and inspects what came out the far end:
// the diagnostics the caller gets, the [audit.FormatDiagnostics] projection, and
// the `config.reload_failed` event as the audit writer received it — the last one
// being literally the row that lands in `audit_log`.
func TestVaultRef_RejectedValueNeverReachesAuditLog(t *testing.T) {
	path := fixtureKeeperPath(t)
	w := &mockWriter{}
	store, _, err := LoadKeeperStoreWithAudit(path, ValidateOptions{}, w)
	if err != nil {
		t.Fatalf("LoadKeeperStoreWithAudit: %v", err)
	}

	if err := os.WriteFile(path, []byte(vaultRefCanaryConfig), 0o644); err != nil {
		t.Fatalf("write canary config: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceSignal)
	if res.Swapped {
		t.Fatalf("Swapped=true — the canary config was accepted, so nothing was rejected and " +
			"the guard has no diagnostic to inspect")
	}

	assertVaultRefCoverage(t, res.Diagnostics)

	// Surface 1: the diagnostics themselves — what the reload caller, `soul-lint`
	// and the startup path all render from, and the source every other surface is
	// derived from. Marshalled whole so Hint and any future field are covered too.
	db, err := json.Marshal(res.Diagnostics)
	if err != nil {
		t.Fatalf("marshal diagnostics: %v", err)
	}
	assertCanaryAbsent(t, "ReloadResult.Diagnostics", string(db))

	// Surface 2: the audit projection in isolation — the ADR-022(j)
	// validation_errors[] shape.
	fb, err := json.Marshal(audit.FormatDiagnostics(res.Diagnostics))
	if err != nil {
		t.Fatalf("marshal FormatDiagnostics: %v", err)
	}
	assertCanaryAbsent(t, "audit.FormatDiagnostics output", string(fb))

	// Surface 3: the durable one. The event as the writer received it is what the
	// `audit_log` INSERT serializes, and that table is append-only with a 365-day
	// retention — a secret that reaches here outlives the incident.
	evs := w.snapshot()
	if len(evs) != 1 {
		t.Fatalf("audit event count = %d, want 1", len(evs))
	}
	if evs[0].EventType != audit.EventConfigReloadFailed {
		t.Fatalf("EventType = %q, want %q", evs[0].EventType, audit.EventConfigReloadFailed)
	}
	if _, ok := evs[0].Payload["validation_errors"]; !ok {
		t.Fatalf("payload has no validation_errors — the durable surface is empty, so the "+
			"absence assertion below would pass vacuously: %#v", evs[0].Payload)
	}
	pb, err := json.Marshal(evs[0].Payload)
	if err != nil {
		t.Fatalf("marshal audit payload: %v", err)
	}
	assertCanaryAbsent(t, "config.reload_failed payload (audit_log row)", string(pb))
}

// TestVaultRef_RejectedValueNeverReachesOverlayReply — the second outbound
// surface, and the reason the guard above is not enough on its own.
//
// The settings API writes overlay entries to Postgres and re-validates the MERGED
// config; on rejection `settingsstore.Refresh` returns
// `merged config rejected: <first error message>` straight to the HTTP caller.
// That path never touches Store.Reload or the audit writer, so a fix applied only
// where the reload renders would still ship the value to an API client. Both
// surfaces read the same [vaultRefMessage], which is what makes one fix cover
// them; this test is what keeps that true.
func TestVaultRef_RejectedValueNeverReachesOverlayReply(t *testing.T) {
	store, src, _ := overlayStore(t, "")

	// One entry per validator, so the test covers both message renderers on this
	// surface too. Both keys are ABSENT from the golden fixture on purpose: the
	// file beats the cluster for a key it sets (ADR-0073(b)), so overlaying a key
	// the fixture already fills would be silently discarded and this test would
	// pass while exercising nothing.
	src.set(
		OverlayEntry{Path: "$.cloud_init.tls_ca_ref", Value: vaultRefCanary},
		OverlayEntry{Path: "$.sigil.signing_key_ref", Value: vaultRefCanary},
	)
	res := store.RefreshOverlay(context.Background())
	if res.Swapped {
		t.Fatalf("Swapped=true — the overlay was accepted, so the guard has no rejection to inspect")
	}

	var refDiags int
	for _, d := range res.Diagnostics {
		if d.Code == "vault_ref_invalid_format" || d.Code == "vault_ref_invalid" {
			refDiags++
		}
	}
	if refDiags != 2 {
		dump(t, res.Diagnostics)
		t.Fatalf("vault-ref diagnostics = %d, want 2 (one per validator) — the overlay did not "+
			"reach both phases, so this surface is not being tested", refDiags)
	}

	// firstErrorMessage() in settingsstore picks exactly this and interpolates it
	// into the error the API returns; marshal them all, since which one is "first"
	// is diagnostic order, not a contract.
	db, err := json.Marshal(res.Diagnostics)
	if err != nil {
		t.Fatalf("marshal diagnostics: %v", err)
	}
	assertCanaryAbsent(t, "RefreshOverlay diagnostics (settings API reply)", string(db))
}

// TestVaultRef_CanaryIsNotItselfAVaultRef pins the canary at the source: if it
// ever started matching the vault-ref grammar, every field above would ACCEPT it,
// coverage would collapse and the guard would report success while testing
// nothing. Cheap check, one clear message.
func TestVaultRef_CanaryIsNotItselfAVaultRef(t *testing.T) {
	if reVaultRef.MatchString(vaultRefCanary) {
		t.Errorf("canary %q matches reVaultRef — semantic-validate would accept it", vaultRefCanary)
	}
	if isVaultRef(vaultRefCanary) {
		t.Errorf("canary %q satisfies isVaultRef — schema-validate would accept it", vaultRefCanary)
	}
}
