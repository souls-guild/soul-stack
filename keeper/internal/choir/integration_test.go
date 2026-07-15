//go:build integration

// Integration tests for Choir/Voice CRUD via testcontainers-go (ADR-044, S-T2).
// Pattern matches keeper/internal/incarnation/integration_test.go.

package choir

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
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
			log.Fatalf("choir integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("choir integration: skipping, docker unavailable: %v", err)
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
	// CASCADE: incarnation_choir_voices / incarnation_choirs → incarnation /
	// souls → operators (FK chain).
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE incarnation_choir_voices, incarnation_choirs,
		 souls, incarnation, operators CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedOperator(t *testing.T, aid string) {
	t.Helper()
	op := &operator.Operator{
		AID:         aid,
		DisplayName: aid,
		AuthMethod:  operator.AuthMethodJWT,
	}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seedOperator(%s): %v", aid, err)
	}
}

func seedIncarnation(t *testing.T, name, creator string) {
	t.Helper()
	inc := &incarnation.Incarnation{
		Name:               name,
		Service:            "redis",
		ServiceVersion:     "v1.0.0",
		StateSchemaVersion: 1,
		Spec:               map[string]any{},
		Status:             incarnation.StatusReady,
		CreatedByAID:       &creator,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnation(%s): %v", name, err)
	}
}

// seedSoul inserts a Soul with the given set of coven tags (membership in
// incarnations = incarnation.name in coven, ADR-008 / ADR-044 item 3).
func seedSoul(t *testing.T, sid string, coven ...string) {
	t.Helper()
	s := &soul.Soul{
		SID:       sid,
		Transport: soul.TransportAgent,
		Status:    soul.StatusConnected,
		Coven:     coven,
	}
	if err := soul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedSoul(%s): %v", sid, err)
	}
}

func iptr(i int) *int       { return &i }
func sptr(s string) *string { return &s }

// --- Choir CRUD ---

func TestIntegration_CreateChoir_AndGet(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	ctx := context.Background()

	c := &Choir{
		IncarnationName: "service-redis",
		ChoirName:       "redis_primary",
		Description:     sptr("primary shard"),
		MinSize:         iptr(1),
		MaxSize:         iptr(3),
		CreatedByAID:    sptr("archon-alice"),
	}
	if err := CreateChoir(ctx, integrationPool, c); err != nil {
		t.Fatalf("CreateChoir: %v", err)
	}
	if c.CreatedAt.IsZero() {
		t.Errorf("CreatedAt not populated by RETURNING")
	}

	got, err := GetChoir(ctx, integrationPool, "service-redis", "redis_primary")
	if err != nil {
		t.Fatalf("GetChoir: %v", err)
	}
	if got.ChoirName != "redis_primary" || got.MinSize == nil || *got.MinSize != 1 || got.MaxSize == nil || *got.MaxSize != 3 {
		t.Errorf("GetChoir mismatch: %+v", got)
	}
}

func TestIntegration_CreateChoir_DuplicateRejected(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	ctx := context.Background()

	c := &Choir{IncarnationName: "service-redis", ChoirName: "workers"}
	if err := CreateChoir(ctx, integrationPool, c); err != nil {
		t.Fatalf("CreateChoir #1: %v", err)
	}
	c2 := &Choir{IncarnationName: "service-redis", ChoirName: "workers"}
	if err := CreateChoir(ctx, integrationPool, c2); !errors.Is(err, ErrChoirExists) {
		t.Errorf("CreateChoir dup: want ErrChoirExists, got %v", err)
	}
}

func TestIntegration_CreateChoir_UnknownIncarnation(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	c := &Choir{IncarnationName: "ghost", ChoirName: "workers"}
	if err := CreateChoir(ctx, integrationPool, c); !errors.Is(err, ErrIncarnationNotFound) {
		t.Errorf("CreateChoir unknown incarnation: want ErrIncarnationNotFound, got %v", err)
	}
}

func TestIntegration_CreateChoir_InvalidName(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	c := &Choir{IncarnationName: "service-redis", ChoirName: "Redis-Primary"}
	if err := CreateChoir(ctx, integrationPool, c); !errors.Is(err, ErrInvalidChoirName) {
		t.Errorf("CreateChoir invalid name: want ErrInvalidChoirName, got %v", err)
	}
}

func TestIntegration_CreateChoir_InvalidSizeBounds(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	c := &Choir{IncarnationName: "service-redis", ChoirName: "workers", MinSize: iptr(5), MaxSize: iptr(2)}
	if err := CreateChoir(ctx, integrationPool, c); !errors.Is(err, ErrInvalidSizeBounds) {
		t.Errorf("CreateChoir bad bounds: want ErrInvalidSizeBounds, got %v", err)
	}
}

func TestIntegration_ListChoirs(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	ctx := context.Background()

	for _, n := range []string{"workers", "redis_primary", "redis_replica"} {
		if err := CreateChoir(ctx, integrationPool, &Choir{IncarnationName: "service-redis", ChoirName: n}); err != nil {
			t.Fatalf("CreateChoir(%s): %v", n, err)
		}
	}
	got, err := ListChoirs(ctx, integrationPool, "service-redis")
	if err != nil {
		t.Fatalf("ListChoirs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListChoirs len=%d, want 3", len(got))
	}
	// ORDER BY choir_name.
	want := []string{"redis_primary", "redis_replica", "workers"}
	for i, c := range got {
		if c.ChoirName != want[i] {
			t.Errorf("ListChoirs[%d]=%q, want %q", i, c.ChoirName, want[i])
		}
	}
}

func TestIntegration_DeleteChoir_CascadesVoices(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	seedSoul(t, "host-a.example.com", "service-redis")
	ctx := context.Background()

	if err := CreateChoir(ctx, integrationPool, &Choir{IncarnationName: "service-redis", ChoirName: "workers"}); err != nil {
		t.Fatalf("CreateChoir: %v", err)
	}
	if err := AddVoice(ctx, integrationPool, &Voice{IncarnationName: "service-redis", ChoirName: "workers", SID: "host-a.example.com"}); err != nil {
		t.Fatalf("AddVoice: %v", err)
	}
	if err := DeleteChoir(ctx, integrationPool, "service-redis", "workers"); err != nil {
		t.Fatalf("DeleteChoir: %v", err)
	}
	// Voice was removed by the cascade — a repeat RemoveVoice finds no row.
	if err := RemoveVoice(ctx, integrationPool, "service-redis", "workers", "host-a.example.com"); !errors.Is(err, ErrVoiceNotFound) {
		t.Errorf("RemoveVoice after cascade: want ErrVoiceNotFound, got %v", err)
	}
}

func TestIntegration_DeleteChoir_NotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	if err := DeleteChoir(ctx, integrationPool, "service-redis", "ghost"); !errors.Is(err, ErrChoirNotFound) {
		t.Errorf("DeleteChoir not found: want ErrChoirNotFound, got %v", err)
	}
}

// --- Voice CRUD + membership invariant ---

func TestIntegration_AddVoice_AndList(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	seedSoul(t, "host-a.example.com", "service-redis")
	seedSoul(t, "host-b.example.com", "service-redis", "prod")
	ctx := context.Background()

	if err := CreateChoir(ctx, integrationPool, &Choir{IncarnationName: "service-redis", ChoirName: "redis_primary"}); err != nil {
		t.Fatalf("CreateChoir: %v", err)
	}
	if err := AddVoice(ctx, integrationPool, &Voice{
		IncarnationName: "service-redis", ChoirName: "redis_primary",
		SID: "host-b.example.com", Role: sptr("master"), Position: iptr(0), AddedByAID: sptr("archon-alice"),
	}); err != nil {
		t.Fatalf("AddVoice b: %v", err)
	}
	if err := AddVoice(ctx, integrationPool, &Voice{
		IncarnationName: "service-redis", ChoirName: "redis_primary",
		SID: "host-a.example.com", Role: sptr("replica"), Position: iptr(1),
	}); err != nil {
		t.Fatalf("AddVoice a: %v", err)
	}

	voices, err := ListVoices(ctx, integrationPool, "service-redis", "redis_primary")
	if err != nil {
		t.Fatalf("ListVoices: %v", err)
	}
	if len(voices) != 2 {
		t.Fatalf("ListVoices len=%d, want 2", len(voices))
	}
	// ORDER BY position NULLS LAST, sid → position 0 (host-b) first.
	if voices[0].SID != "host-b.example.com" || voices[0].Role == nil || *voices[0].Role != "master" {
		t.Errorf("voices[0] mismatch: %+v", voices[0])
	}
	if voices[1].SID != "host-a.example.com" {
		t.Errorf("voices[1]=%q, want host-a.example.com", voices[1].SID)
	}
}

func TestIntegration_AddVoice_DuplicateRejected(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	seedSoul(t, "host-a.example.com", "service-redis")
	ctx := context.Background()

	if err := CreateChoir(ctx, integrationPool, &Choir{IncarnationName: "service-redis", ChoirName: "workers"}); err != nil {
		t.Fatalf("CreateChoir: %v", err)
	}
	v := &Voice{IncarnationName: "service-redis", ChoirName: "workers", SID: "host-a.example.com"}
	if err := AddVoice(ctx, integrationPool, v); err != nil {
		t.Fatalf("AddVoice #1: %v", err)
	}
	v2 := &Voice{IncarnationName: "service-redis", ChoirName: "workers", SID: "host-a.example.com"}
	if err := AddVoice(ctx, integrationPool, v2); !errors.Is(err, ErrVoiceExists) {
		t.Errorf("AddVoice dup: want ErrVoiceExists, got %v", err)
	}
}

func TestIntegration_AddVoice_ChoirNotFound(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	seedSoul(t, "host-a.example.com", "service-redis")
	ctx := context.Background()

	v := &Voice{IncarnationName: "service-redis", ChoirName: "ghost", SID: "host-a.example.com"}
	if err := AddVoice(ctx, integrationPool, v); !errors.Is(err, ErrChoirNotFound) {
		t.Errorf("AddVoice no choir: want ErrChoirNotFound, got %v", err)
	}
}

// ADR-044 item 3 invariant: a Voice only for a SID that is ALREADY a member of
// the incarnation (souls.coven contains incarnation.name). SID exists in
// souls, but coven doesn't carry the incarnation → ErrNotMembers.
func TestIntegration_AddVoice_NonMemberRejected(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	// host-x is a member of a DIFFERENT incarnation, not service-redis.
	seedSoul(t, "host-x.example.com", "service-haproxy", "prod")
	ctx := context.Background()

	if err := CreateChoir(ctx, integrationPool, &Choir{IncarnationName: "service-redis", ChoirName: "workers"}); err != nil {
		t.Fatalf("CreateChoir: %v", err)
	}
	v := &Voice{IncarnationName: "service-redis", ChoirName: "workers", SID: "host-x.example.com"}
	err := AddVoice(ctx, integrationPool, v)
	var notMembers *ErrNotMembers
	if !errors.As(err, &notMembers) {
		t.Fatalf("AddVoice non-member: want *ErrNotMembers, got %v", err)
	}
	if len(notMembers.Missing) != 1 || notMembers.Missing[0] != "host-x.example.com" {
		t.Errorf("ErrNotMembers.Missing = %v, want [host-x.example.com]", notMembers.Missing)
	}
}

// SID absent from the souls registry entirely → also ErrNotMembers (the
// invariant is stricter than the FK on souls — the membership check runs
// before the INSERT).
func TestIntegration_AddVoice_UnknownSIDRejected(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	ctx := context.Background()

	if err := CreateChoir(ctx, integrationPool, &Choir{IncarnationName: "service-redis", ChoirName: "workers"}); err != nil {
		t.Fatalf("CreateChoir: %v", err)
	}
	v := &Voice{IncarnationName: "service-redis", ChoirName: "workers", SID: "nope.example.com"}
	if err := AddVoice(ctx, integrationPool, v); !errors.As(err, new(*ErrNotMembers)) {
		t.Errorf("AddVoice unknown sid: want *ErrNotMembers, got %v", err)
	}
}

// Multi-incarnation membership (ADR-044 item 3): one SID can legally be a
// Voice in Choirs of DIFFERENT incarnations (its coven carries both). The PK
// (incarnation, choir, sid) supports this; there is no global UNIQUE(sid).
func TestIntegration_AddVoice_MultiIncarnation(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	seedIncarnation(t, "service-haproxy", "archon-alice")
	// host-a is a member of BOTH incarnations.
	seedSoul(t, "host-a.example.com", "service-redis", "service-haproxy")
	ctx := context.Background()

	if err := CreateChoir(ctx, integrationPool, &Choir{IncarnationName: "service-redis", ChoirName: "cache"}); err != nil {
		t.Fatalf("CreateChoir redis: %v", err)
	}
	if err := CreateChoir(ctx, integrationPool, &Choir{IncarnationName: "service-haproxy", ChoirName: "frontends"}); err != nil {
		t.Fatalf("CreateChoir haproxy: %v", err)
	}
	if err := AddVoice(ctx, integrationPool, &Voice{IncarnationName: "service-redis", ChoirName: "cache", SID: "host-a.example.com"}); err != nil {
		t.Fatalf("AddVoice redis: %v", err)
	}
	// Same SID — a Voice in the SECOND incarnation: ok (must not be ErrVoiceExists).
	if err := AddVoice(ctx, integrationPool, &Voice{IncarnationName: "service-haproxy", ChoirName: "frontends", SID: "host-a.example.com"}); err != nil {
		t.Fatalf("AddVoice haproxy (multi-incarnation must be allowed): %v", err)
	}

	rv, err := ListVoices(ctx, integrationPool, "service-redis", "cache")
	if err != nil || len(rv) != 1 {
		t.Fatalf("ListVoices redis: len=%d err=%v", len(rv), err)
	}
	hv, err := ListVoices(ctx, integrationPool, "service-haproxy", "frontends")
	if err != nil || len(hv) != 1 {
		t.Fatalf("ListVoices haproxy: len=%d err=%v", len(hv), err)
	}
}

func TestIntegration_RemoveVoice(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "service-redis", "archon-alice")
	seedSoul(t, "host-a.example.com", "service-redis")
	ctx := context.Background()

	if err := CreateChoir(ctx, integrationPool, &Choir{IncarnationName: "service-redis", ChoirName: "workers"}); err != nil {
		t.Fatalf("CreateChoir: %v", err)
	}
	if err := AddVoice(ctx, integrationPool, &Voice{IncarnationName: "service-redis", ChoirName: "workers", SID: "host-a.example.com"}); err != nil {
		t.Fatalf("AddVoice: %v", err)
	}
	if err := RemoveVoice(ctx, integrationPool, "service-redis", "workers", "host-a.example.com"); err != nil {
		t.Fatalf("RemoveVoice: %v", err)
	}
	if err := RemoveVoice(ctx, integrationPool, "service-redis", "workers", "host-a.example.com"); !errors.Is(err, ErrVoiceNotFound) {
		t.Errorf("RemoveVoice twice: want ErrVoiceNotFound, got %v", err)
	}
}
