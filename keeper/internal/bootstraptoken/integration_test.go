//go:build integration

package bootstraptoken

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("bootstraptoken integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("bootstraptoken integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

func resetAll(t *testing.T) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, souls, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedSoul(t *testing.T, sid string) {
	t.Helper()
	s := &soul.Soul{SID: sid, Status: soul.StatusPending}
	if err := soul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedSoul(%s): %v", sid, err)
	}
}

func TestIntegration_InsertAndBurn_HappyPath(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()

	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	rec, err := Insert(ctx, integrationPool, "host1.example.com", tok.Hash(), 24*time.Hour, nil)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if rec.TokenID == "" {
		t.Errorf("TokenID empty")
	}

	id, err := Burn(ctx, integrationPool, tok.Hash(), "host1.example.com", "kid-1")
	if err != nil {
		t.Fatalf("Burn: %v", err)
	}
	if id != rec.TokenID {
		t.Errorf("burn id = %q, want %q", id, rec.TokenID)
	}

	// Повторный Burn — токен уже сожжён, должен дать ErrTokenInvalid.
	if _, err := Burn(ctx, integrationPool, tok.Hash(), "host1.example.com", "kid-1"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("second Burn err = %v, want ErrTokenInvalid", err)
	}
}

func TestIntegration_Insert_RejectsDuplicateActive(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()

	tok1, _ := Generate()
	if _, err := Insert(ctx, integrationPool, "host1.example.com", tok1.Hash(), 24*time.Hour, nil); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	tok2, _ := Generate()
	_, err := Insert(ctx, integrationPool, "host1.example.com", tok2.Hash(), 24*time.Hour, nil)
	if !errors.Is(err, ErrTokenActiveExists) {
		t.Errorf("err = %v, want ErrTokenActiveExists", err)
	}
}

func TestIntegration_Insert_RejectsNonExistentSoul(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	tok, _ := Generate()
	_, err := Insert(ctx, integrationPool, "ghost.example.com", tok.Hash(), 24*time.Hour, nil)
	if !errors.Is(err, ErrTokenSoulNotFound) {
		t.Errorf("err = %v, want ErrTokenSoulNotFound", err)
	}
}

func TestIntegration_Burn_RejectsWrongSID(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	seedSoul(t, "host2.example.com")
	ctx := context.Background()
	tok, _ := Generate()
	if _, err := Insert(ctx, integrationPool, "host1.example.com", tok.Hash(), 24*time.Hour, nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Тот же hash, но другой SID — Burn должен отвергнуть.
	_, err := Burn(ctx, integrationPool, tok.Hash(), "host2.example.com", "kid-1")
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("err = %v, want ErrTokenInvalid (cross-SID protection)", err)
	}
	// Токен должен остаться активным после неудачной попытки.
	rec, err := SelectByHash(ctx, integrationPool, tok.Hash())
	if err != nil {
		t.Fatalf("SelectByHash: %v", err)
	}
	if rec.UsedAt != nil {
		t.Errorf("UsedAt = %v after failed cross-SID burn, want nil", rec.UsedAt)
	}
}

func TestIntegration_Burn_RejectsExpired(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()
	tok, _ := Generate()
	rec, err := Insert(ctx, integrationPool, "host1.example.com", tok.Hash(), time.Hour, nil)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Принудительно ставим expires_at в прошлое. CHECK
	// `bootstrap_tokens_expires_after_created` требует expires_at > created_at,
	// поэтому смещаем оба назад на сутки (expires_at остаётся минимум на
	// миллисекунду позже created_at, что constraint допускает).
	_, err = integrationPool.Exec(ctx,
		`UPDATE bootstrap_tokens
		    SET created_at = NOW() - INTERVAL '1 hour',
		        expires_at = NOW() - INTERVAL '1 second'
		  WHERE token_id = $1`,
		rec.TokenID,
	)
	if err != nil {
		t.Fatalf("force-expire UPDATE: %v", err)
	}
	_, err = Burn(ctx, integrationPool, tok.Hash(), "host1.example.com", "kid-1")
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("err = %v, want ErrTokenInvalid (expired)", err)
	}
}

func TestIntegration_CascadeOnSoulDelete(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()
	tok, _ := Generate()
	if _, err := Insert(ctx, integrationPool, "host1.example.com", tok.Hash(), 24*time.Hour, nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if _, err := integrationPool.Exec(ctx, `DELETE FROM souls WHERE sid = $1`, "host1.example.com"); err != nil {
		t.Fatalf("DELETE soul: %v", err)
	}
	// CASCADE: токен должен исчезнуть.
	_, err := SelectByHash(ctx, integrationPool, tok.Hash())
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("err = %v, want ErrTokenNotFound after CASCADE", err)
	}
}

func TestIntegration_BurnAllForSID(t *testing.T) {
	// ADR-017 cascade: BurnAllForSID жжёт все ещё-активные токены SID
	// и пишет SystemKIDCloudDestroy в used_by_kid.
	resetAll(t)
	seedSoul(t, "host-burn.example.com")
	ctx := context.Background()
	tok, _ := Generate()
	if _, err := Insert(ctx, integrationPool, "host-burn.example.com", tok.Hash(), 24*time.Hour, nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	n, err := BurnAllForSID(ctx, integrationPool, "host-burn.example.com", SystemKIDCloudDestroy)
	if err != nil {
		t.Fatalf("BurnAllForSID: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	rec, err := SelectByHash(ctx, integrationPool, tok.Hash())
	if err != nil {
		t.Fatalf("SelectByHash: %v", err)
	}
	if rec.UsedAt == nil {
		t.Error("UsedAt is NULL after BurnAllForSID")
	}
	if rec.UsedByKID == nil || *rec.UsedByKID != SystemKIDCloudDestroy {
		t.Errorf("UsedByKID = %v, want %q", rec.UsedByKID, SystemKIDCloudDestroy)
	}

	// Повторный вызов — уже сожжённый токен не трогается (rows=0).
	n2, err := BurnAllForSID(ctx, integrationPool, "host-burn.example.com", SystemKIDCloudDestroy)
	if err != nil {
		t.Fatalf("BurnAllForSID 2nd: %v", err)
	}
	if n2 != 0 {
		t.Errorf("rows on 2nd call = %d, want 0 (no active tokens)", n2)
	}
}

func TestIntegration_DeleteByTokenID(t *testing.T) {
	resetAll(t)
	seedSoul(t, "host1.example.com")
	ctx := context.Background()
	tok, _ := Generate()
	rec, err := Insert(ctx, integrationPool, "host1.example.com", tok.Hash(), 24*time.Hour, nil)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := DeleteByTokenID(ctx, integrationPool, rec.TokenID); err != nil {
		t.Fatalf("DeleteByTokenID: %v", err)
	}
	if err := DeleteByTokenID(ctx, integrationPool, rec.TokenID); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("second DeleteByTokenID err = %v, want ErrTokenNotFound", err)
	}
}
