package handlers

// Guard-leak test for the AUDIT sink (ADR-064 mitigation b, NIM-11): under dual-mode
// plaintext ingestion the secret does NOT reach the audit-payload of the Herald
// write routes; the plaintext_ingested marker is present (without a value). Full path:
// handler → service → materialization → reply.AuditPayload.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

const leakHandlerPlaintext = "PLAINTEXT-HANDLER-SECRET-1a2b3c"

// leakVault implements herald.SecretWriter (WriteString) — keeper-side
// materialization into Vault.
type leakVault struct{}

func (leakVault) WriteString(_ context.Context, domain, entity, field, _ string) (string, error) {
	return "vault:secret/" + domain + "/" + entity + "/" + field + "#" + field, nil
}

// leakPool — an ExecQueryRower (herald): QueryRow returns scannable timestamps,
// Exec succeeds.
type leakPool struct{}

func (leakPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func (leakPool) QueryRow(context.Context, string, ...any) pgx.Row { return leakTSRow{} }
func (leakPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, context.Canceled
}

type leakTSRow struct{}

func (leakTSRow) Scan(dst ...any) error {
	for _, d := range dst {
		if p, ok := d.(*time.Time); ok {
			*p = time.Unix(0, 0)
		}
	}
	return nil
}

// TestHeraldAuditNoPlaintext — audit-payload of the Herald create route without plaintext.
func TestHeraldAuditNoPlaintext(t *testing.T) {
	svc, err := herald.NewService(herald.ServiceDeps{
		Pool: leakPool{}, SecretWriter: leakVault{}, AcceptPlaintext: true,
	})
	if err != nil {
		t.Fatalf("herald.NewService: %v", err)
	}
	h := NewHeraldHandler(svc, nil)
	sec := leakHandlerPlaintext
	reply, err := h.CreateHeraldTyped(context.Background(), &keeperjwt.Claims{Subject: "archon-test"},
		HeraldCreateInput{
			ID:     "ops-hook",
			Type:   "webhook",
			Config: map[string]any{"url": "https://example.com/hook"},
			Secret: &sec,
		})
	if err != nil {
		t.Fatalf("CreateHeraldTyped: %v", err)
	}

	auditJSON, _ := json.Marshal(map[string]any(reply.AuditPayload()))
	if strings.Contains(string(auditJSON), leakHandlerPlaintext) {
		t.Fatalf("plaintext leaked into herald audit-payload: %s", auditJSON)
	}
	if !strings.Contains(string(auditJSON), "plaintext_ingested") {
		t.Fatalf("plaintext_ingested marker missing from audit: %s", auditJSON)
	}
	// The returned View is clean too.
	if viewJSON, _ := json.Marshal(reply.View); strings.Contains(string(viewJSON), leakHandlerPlaintext) {
		t.Fatalf("plaintext leaked into herald View: %s", viewJSON)
	}
}
