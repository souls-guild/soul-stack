//go:build smoke

package migrate_test

// Ad-hoc smoke test for M0.4.0: start local postgres (through `make dev-up` or
// `docker run`), then run
//
//   SOUL_STACK_SMOKE_DSN="postgres://keeper:keeper@localhost:55432/keeper?sslmode=disable" \
//     go test -tags=smoke ./internal/migrate/ -run TestSmoke
//
// Test:
//   1. NewPool on DSN.
//   2. Ping.
//   3. migrate.Apply (migration 001_create_audit_log).
//   4. auditpg.NewWriter + Write one config.reload_succeeded.
//   5. SELECT back and verify masking.
//
// Full integration tests under testcontainers are M0.4.1.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	keepermigrate "github.com/souls-guild/soul-stack/keeper/internal/migrate"
	keeperpg "github.com/souls-guild/soul-stack/keeper/internal/pg"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

func TestSmoke_EndToEnd(t *testing.T) {
	dsn := os.Getenv("SOUL_STACK_SMOKE_DSN")
	if dsn == "" {
		t.Skip("SOUL_STACK_SMOKE_DSN not set; skip smoke")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := keeperpg.NewPool(ctx, config.KeeperPostgres{
		DSNRef: dsn,
		Pool:   config.KeeperPostgresPool{Min: 1, Max: 4},
	}, nil) // plain DSN; vc is not required
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	if err := keeperpg.Ping(ctx, pool); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if err := keepermigrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	writer := auditpg.NewWriter(pool)
	corrID := audit.NewULID()
	ev := &audit.Event{
		EventType:     audit.EventConfigReloadSucceeded,
		Source:        audit.SourceSignal,
		CorrelationID: corrID,
		Payload: map[string]any{
			"path":      "/etc/keeper.yml",
			"password":  "should-be-masked",
			"vault_ref": "vault:secret/keeper/postgres",
		},
	}
	if err := writer.Write(ctx, ev); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// SELECT back: verify payload and column scaling.
	row := pool.QueryRow(ctx, `
		SELECT audit_id, event_type, source, archon_aid, correlation_id, payload
		FROM audit_log
		WHERE correlation_id = $1
	`, corrID)
	var (
		auditID       string
		eventType     string
		source        string
		archonAID     *string
		correlationID string
		payloadBytes  []byte
	)
	if err := row.Scan(&auditID, &eventType, &source, &archonAID, &correlationID, &payloadBytes); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if eventType != "config.reload_succeeded" {
		t.Errorf("event_type = %q", eventType)
	}
	if source != "signal" {
		t.Errorf("source = %q", source)
	}
	if archonAID != nil {
		t.Errorf("archon_aid = %v, want NULL for signal", *archonAID)
	}
	if correlationID != corrID {
		t.Errorf("correlation_id roundtrip mismatch: %q vs %q", correlationID, corrID)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, payloadBytes)
	}
	if payload["password"] != "***MASKED***" {
		t.Errorf("payload.password = %v, want masked", payload["password"])
	}
	if payload["vault_ref"] != "***MASKED***" {
		t.Errorf("payload.vault_ref = %v, want masked", payload["vault_ref"])
	}
	if payload["path"] != "/etc/keeper.yml" {
		t.Errorf("payload.path = %v, want passthrough", payload["path"])
	}
}
