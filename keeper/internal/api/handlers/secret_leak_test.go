package handlers

// Guard-leak тест sink-а AUDIT (ADR-064 митигация b, NIM-11): при dual-mode
// plaintext-приёме секрет НЕ попадает в audit-payload write-роутов Herald/
// Provider; маркер plaintext_ingested присутствует (без значения). Полный путь
// handler → service → материализация → reply.AuditPayload.

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
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
)

const leakHandlerPlaintext = "PLAINTEXT-HANDLER-SECRET-1a2b3c"

// leakVault реализует и herald.SecretWriter (WriteString), и provider.SecretWriter
// (WriteMap) — keeper-side материализация в Vault.
type leakVault struct{}

func (leakVault) WriteString(_ context.Context, domain, entity, field, _ string) (string, error) {
	return "vault:secret/" + domain + "/" + entity + "/" + field + "#" + field, nil
}

func (leakVault) WriteMap(_ context.Context, domain, entity, field string, _ map[string]any) (string, error) {
	return "vault:secret/" + domain + "/" + entity + "/" + field, nil
}

// leakPool — ExecQueryRower (herald + provider): QueryRow отдаёт scannable
// timestamps, Exec — успех.
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

// TestHeraldAuditNoPlaintext — audit-payload create-роута Herald БЕЗ plaintext.
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
			Name:   "ops-hook",
			Type:   "webhook",
			Config: map[string]any{"url": "https://example.com/hook"},
			Secret: &sec,
		})
	if err != nil {
		t.Fatalf("CreateHeraldTyped: %v", err)
	}

	auditJSON, _ := json.Marshal(map[string]any(reply.AuditPayload()))
	if strings.Contains(string(auditJSON), leakHandlerPlaintext) {
		t.Fatalf("plaintext утёк в herald audit-payload: %s", auditJSON)
	}
	if !strings.Contains(string(auditJSON), "plaintext_ingested") {
		t.Fatalf("маркер plaintext_ingested отсутствует в audit: %s", auditJSON)
	}
	// Возвращаемый View тоже чист.
	if viewJSON, _ := json.Marshal(reply.View); strings.Contains(string(viewJSON), leakHandlerPlaintext) {
		t.Fatalf("plaintext утёк в herald View: %s", viewJSON)
	}
}

// TestProviderAuditNoPlaintext — audit-payload create-роута Provider БЕЗ plaintext.
func TestProviderAuditNoPlaintext(t *testing.T) {
	svc, err := provider.NewService(provider.ServiceDeps{
		Pool: leakPool{}, SecretWriter: leakVault{}, AcceptPlaintext: true,
	})
	if err != nil {
		t.Fatalf("provider.NewService: %v", err)
	}
	h := NewProviderHandler(svc, nil)
	reply, err := h.CreateTyped(context.Background(), &keeperjwt.Claims{Subject: "archon-test"},
		ProviderCreateInput{
			Name:        "aws-prod",
			Type:        "aws",
			Region:      "eu-west-1",
			Credentials: map[string]any{"secret_key": leakHandlerPlaintext},
		})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}

	auditJSON, _ := json.Marshal(map[string]any(reply.AuditPayload()))
	if strings.Contains(string(auditJSON), leakHandlerPlaintext) {
		t.Fatalf("plaintext утёк в provider audit-payload: %s", auditJSON)
	}
	if !strings.Contains(string(auditJSON), "plaintext_ingested") {
		t.Fatalf("маркер plaintext_ingested отсутствует в audit: %s", auditJSON)
	}
	if bodyJSON, _ := json.Marshal(reply.Body); strings.Contains(string(bodyJSON), leakHandlerPlaintext) {
		t.Fatalf("plaintext утёк в provider View: %s", bodyJSON)
	}
}
