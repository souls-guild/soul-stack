package incarnation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// NIM-531 guard: a force destroy must not print a state field the service
// declared `secret: true`.
//
// The canary is chosen so that ONLY the declarative layer can catch it — this is
// the whole point, and a canary that any other layer would also catch would make
// the test pass with the fix reverted:
//
//   - the value is not a vault ref, so layer 2 (vault origin, by content) is
//     silent — `unreleasedCanary` contains no `vault:` prefix;
//   - the key is `provisioned_provider`, which matches none of the fragments in
//     audit's sensitiveKeyRe (token/secret/password/…/access_key/tls_*), so layer
//     4 (regex-last-resort, by key name) is silent. TestUnreleasedSecret_CanaryIsInvisibleToNonSchemaLayers
//     below pins that, so a later widening of the regex turns into a visible
//     failure here rather than a guard that silently stops testing its subject.
const (
	unreleasedCanary   = "nim531-canary-Xk29fLm-do-not-log"
	unreleasedCanaryVM = "nim531-canary-vm-Qw81zRt"
)

// canaryStateJSON is the `incarnation.state` a force destroy is about to abandon:
// both keys collectUnreleased reads carry a canary, and the provider key is the
// one the schema declares secret.
const canaryStateJSON = `{
  "provisioned_provider": "` + unreleasedCanary + `",
  "provisioned_vm_ids": ["` + unreleasedCanaryVM + `"],
  "provisioned_sids": ["vm-1.example.com"]
}`

// canarySecretSchema is the declarative layer a caller builds from the service
// artifact: `provisioned_provider` is secret, `provisioned_vm_ids` deliberately
// is NOT — an over-masking fix that blanks everything would pass a
// canary-is-absent assertion, so the test also demands the non-secret canary
// still comes through.
func canarySecretSchema(t *testing.T) audit.SecretSchema {
	t.Helper()
	art := &artifact.ServiceArtifact{Manifest: &config.ServiceManifest{
		StateSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"provisioned_provider": map[string]any{"type": "string", "secret": true},
				"provisioned_vm_ids": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
			},
		},
	}}
	s := StateSchemaSecrets(art)
	if s == nil {
		t.Fatal("StateSchemaSecrets returned nil for a state_schema that declares a secret — " +
			"the guard would then be asserting nothing")
	}
	if !s.IsSecret("provisioned_provider") {
		t.Fatal("StateSchemaSecrets did not collect `provisioned_provider` — the guard's subject is missing")
	}
	return s
}

// canaryDeleteTx — a winning force-delete transaction over canaryStateJSON.
func canaryDeleteTx() *fakeTx {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	tx.selectRow = scriptedRow{values: []any{[]byte(canaryStateJSON)}}
	tx.rowsResult = &fakeRows{rows: []staticRow{{values: []any{"vm-1.example.com"}}}}
	return tx
}

// surfacesJSON renders every durable/outbound surface a force destroy produces
// into one searchable blob: the reply record (which is verbatim what the DELETE
// body, the MCP structuredContent and the WARN line are built from), the audit
// event payloads, and the args of every Exec in the delete transaction — the
// third of which is the `status_details` patch that lands in
// incarnation_archive. Searching the raw SQL args rather than a re-marshalled
// struct is deliberate: the archive is what outlives the incarnation, so the
// assertion has to read what was actually sent to Postgres.
func surfacesJSON(t *testing.T, res *DeleteResult, aw *fakeAuditWriter, tx *fakeTx) map[string]string {
	t.Helper()
	out := map[string]string{}

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal DeleteResult: %v", err)
	}
	out["reply/mcp/log record"] = string(b)

	var evs []string
	for _, e := range aw.events {
		eb, err := json.Marshal(e.Payload)
		if err != nil {
			t.Fatalf("marshal audit payload: %v", err)
		}
		evs = append(evs, string(eb))
	}
	out["audit_log payload"] = strings.Join(evs, "\n")

	var args []string
	for _, a := range tx.execArgs {
		for _, v := range a {
			switch x := v.(type) {
			case []byte:
				args = append(args, string(x))
			case string:
				args = append(args, x)
			default:
				ab, _ := json.Marshal(x)
				args = append(args, string(ab))
			}
		}
	}
	out["incarnation_archive INSERT args"] = strings.Join(args, "\n")

	return out
}

// TestDeleteAfterTeardown_ForceDoesNotLeakSchemaDeclaredSecret — NIM-531 GUARD.
// A state key the service declared `secret: true` must not reach any of the four
// surfaces a force destroy writes to. The audit trail and the compliance archive
// are the ones that matter: they are durable, and nothing prunes them on the
// timescale an incident is investigated on.
func TestDeleteAfterTeardown_ForceDoesNotLeakSchemaDeclaredSecret(t *testing.T) {
	tx := canaryDeleteTx()
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	res, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", true, canarySecretSchema(t), nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if res == nil || res.Unreleased == nil {
		t.Fatal("Unreleased = nil — this force abandoned resources; the guard has nothing to inspect")
	}

	surfaces := surfacesJSON(t, res, aw, tx)

	// Non-vacuity floor: the surfaces must actually carry the record. Without
	// this, a change that stops publishing `unreleased` at all would make every
	// absence assertion below pass while removing the operator's only record of
	// abandoned cloud VMs.
	for name, blob := range surfaces {
		if !strings.Contains(blob, unreleasedCanaryVM) {
			t.Fatalf("%s does not carry the non-secret vm id %q — the surface is empty, "+
				"so the secret-absent assertions below would pass vacuously:\n%s",
				name, unreleasedCanaryVM, blob)
		}
	}

	for name, blob := range surfaces {
		if strings.Contains(blob, unreleasedCanary) {
			t.Errorf("%s contains the schema-declared secret %q in clear text:\n%s",
				name, unreleasedCanary, blob)
		}
		if !strings.Contains(blob, audit.MaskedValue) {
			t.Errorf("%s carries neither the secret nor %s — the field was dropped instead of masked, "+
				"which hides from the operator that a value was there at all:\n%s",
				name, audit.MaskedValue, blob)
		}
	}
}

// TestDeleteAfterTeardown_ForceWithoutSchemaLeaksTheCanary — the negative
// control for the guard above, and the reason it is not passing by accident.
//
// nil secrets is the documented degradation (unreadable artifact → vault+regex
// only) and is exactly the code shape the fix replaced: before NIM-531
// collectUnreleased had no schema to pass. This test pins that the canary DOES
// come through on that path, which is what makes the guard's absence assertion
// mean "the declarative layer masked it" rather than "nothing was there".
//
// It is also the mutation gate: reverting collectUnreleased to
// audit.MaskSecrets(state) makes the guard above fail while this one still
// passes, so the pair localizes the break instead of just reporting one.
func TestDeleteAfterTeardown_ForceWithoutSchemaLeaksTheCanary(t *testing.T) {
	tx := canaryDeleteTx()
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	res, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", true, nil, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}

	for name, blob := range surfacesJSON(t, res, aw, tx) {
		if !strings.Contains(blob, unreleasedCanary) {
			t.Errorf("%s masked the canary WITHOUT a schema — some other layer catches it, so the "+
				"schema guard is not testing the declarative layer. Pick a canary key/value no other "+
				"layer sees:\n%s", name, blob)
		}
	}
}

// TestUnreleasedSecret_CanaryIsInvisibleToNonSchemaLayers pins the canary's
// choice at the source instead of at the destroy path: neither the vault-origin
// layer (by value) nor the regex-last-resort layer (by key name) may see it. If
// audit's sensitiveKeyRe ever grows a fragment that matches `provisioned_provider`,
// this fails here — one clear message — rather than turning the NIM-531 guard
// into a test that passes for the wrong reason.
func TestUnreleasedSecret_CanaryIsInvisibleToNonSchemaLayers(t *testing.T) {
	state := map[string]any{
		stateKeyProvisionedProvider: unreleasedCanary,
		stateKeyProvisionedVMIDs:    []any{unreleasedCanaryVM},
	}
	masked := audit.MaskSecrets(state)
	if got, _ := masked[stateKeyProvisionedProvider].(string); got != unreleasedCanary {
		t.Errorf("vault+regex layers already mask %s=%q (got %q) — the canary no longer isolates the schema layer",
			stateKeyProvisionedProvider, unreleasedCanary, got)
	}
}
