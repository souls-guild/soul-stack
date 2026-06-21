package grpc

// Guard-тесты presence-gated force-release SID-lease (ADR-027 amend (n), S2).
//
// Покрывают ветку [eventStreamHandler.acquireSoulLease] на ErrLeaseTaken:
// перезахват lease у ДОКАЗАННО-МЁРТВОГО prev-holder-а (force-release) против
// сохранения AlreadyExists (split-brain guard / fail-safe). Это
// security-чувствительная операция перехвата владения — каждый инвариант
// зафиксирован тестом, ловящим регресс.
//
// Уровень — unit (miniredis в процессе, без PG): acquireSoulLease зависит
// только от Redis (lease + Conclave-presence) и AuditWriter.

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// newForceLeaseHandler собирает handler с miniredis-Redis-ом, заданным KID-ом
// и capture-audit-ом — общий boilerplate всех force-release-тестов.
func newForceLeaseHandler(t *testing.T, kid string) (*eventStreamHandler, *keeperredis.Client, *captureAudit) {
	t.Helper()
	rc := newClusterRedis(t)
	ca := &captureAudit{}
	h := newEventStreamHandler(EventStreamDeps{
		SeedDB:       &fakeSeedDB{},
		Redis:        rc,
		AuditWriter:  ca,
		KID:          kid,
		SoulLeaseTTL: 5 * time.Second,
	}, discardLogger(t))
	return h, rc, ca
}

// markInstanceAlive регистрирует Conclave-presence-запись KID-а (живой
// keeper-инстанс), чтобы InstanceAlive(kid) вернул true.
func markInstanceAlive(t *testing.T, ctx context.Context, rc *keeperredis.Client, kid string) {
	t.Helper()
	if err := keeperredis.RegisterInstance(ctx, rc, kid, kid, 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance(%s): %v", kid, err)
	}
}

// TestAcquireSoulLease_DeadPrevHolder_ForceReleases — prevKID мёртв в Conclave
// (presence-ключа нет) → force-release: lease перезахвачена на собственный KID,
// cleanup не-nil, ошибки нет (стрим живёт), эмитится audit
// `eventstream.lease_force_released`.
func TestAcquireSoulLease_DeadPrevHolder_ForceReleases(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	// Мёртвый prev-holder держит lease (TTL не истёк после crash-а), но его
	// presence-записи в Conclave НЕТ → доказанно мёртв.
	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-dead", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if err != nil {
		t.Fatalf("acquireSoulLease: err = %v, want nil (force-release должен был перехватить)", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup = nil on successful force-release")
	}
	defer cleanup()

	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-self" {
		t.Errorf("lease owner = %q, want kid-self (перезахвачен)", owner)
	}

	evs := ca.snapshot()
	if len(evs) != 1 {
		t.Fatalf("audit count = %d, want 1 (eventstream.lease_force_released)", len(evs))
	}
	ev := evs[0]
	if ev.EventType != audit.EventLeaseForceReleased {
		t.Errorf("audit event_type = %q, want %q", ev.EventType, audit.EventLeaseForceReleased)
	}
	if ev.Source != audit.SourceSoulGRPC {
		t.Errorf("audit source = %q, want %q", ev.Source, audit.SourceSoulGRPC)
	}
	if ev.Payload["sid"] != sid {
		t.Errorf("audit payload.sid = %v, want %q", ev.Payload["sid"], sid)
	}
	if ev.Payload["prev_kid"] != "kid-dead" {
		t.Errorf("audit payload.prev_kid = %v, want kid-dead", ev.Payload["prev_kid"])
	}
	if ev.Payload["new_kid"] != "kid-self" {
		t.Errorf("audit payload.new_kid = %v, want kid-self", ev.Payload["new_kid"])
	}
}

// TestAcquireSoulLease_LivePrevHolder_NoForce — prevKID ЖИВ в Conclave
// (presence-ключ есть) → НЕ force: AlreadyExists, lease не тронут, audit пуст.
// Защита от split-brain: живого holder-а (или partition-с-живым-Conclave) не
// перехватываем.
func TestAcquireSoulLease_LivePrevHolder_NoForce(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-live", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	markInstanceAlive(t, ctx, rc, "kid-live")

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if cleanup != nil {
		t.Error("cleanup != nil, want nil (lease не захвачен)")
	}
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (split-brain guard)", got)
	}
	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-live" {
		t.Errorf("lease owner = %q, want kid-live (не перезахвачен)", owner)
	}
	if n := len(ca.snapshot()); n != 0 {
		t.Errorf("audit count = %d, want 0 (force не происходил)", n)
	}
}

// TestAcquireSoulLease_PresenceCheckError_FailSafeNoForce — InstanceAlive вернул
// ОШИБКУ (флап Redis на presence-чеке) → fail-safe: НЕ объявлять мёртвым, НЕ
// force → AlreadyExists. По неопределённости lease не перехватывается.
func TestAcquireSoulLease_PresenceCheckError_FailSafeNoForce(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-unknown", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	// Presence-чек падает: подменяем seam на ошибку (эмуляция флапа Redis
	// именно на EXISTS conclave-ключа, без сноса всего miniredis).
	h.instanceAlive = func(context.Context, *keeperredis.Client, string) (bool, error) {
		return false, errors.New("redis flap on EXISTS")
	}

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if cleanup != nil {
		t.Error("cleanup != nil, want nil (fail-safe: lease не захвачен)")
	}
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (fail-safe)", got)
	}
	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-unknown" {
		t.Errorf("lease owner = %q, want kid-unknown (не перезахвачен)", owner)
	}
	if n := len(ca.snapshot()); n != 0 {
		t.Errorf("audit count = %d, want 0 (force не происходил)", n)
	}
}

// TestAcquireSoulLease_PrevHolderIsSelf_NoForce — prevKID == собственный KID
// (reconnect к тому же keeper-у / своя lease) → НЕ force, текущее поведение
// AlreadyExists. Защита от ложного перехвата своей же lease.
func TestAcquireSoulLease_PrevHolderIsSelf_NoForce(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	// lease уже держит ЭТОТ же keeper-инстанс (kid-self).
	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-self", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	// Conclave-presence self не регистрируем намеренно: ветка self обязана
	// сработать ДО presence-чека (иначе self ложно «мёртв» → force своей lease).

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if cleanup != nil {
		t.Error("cleanup != nil, want nil")
	}
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (self не перехватывается)", got)
	}
	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-self" {
		t.Errorf("lease owner = %q, want kid-self (не тронут)", owner)
	}
	if n := len(ca.snapshot()); n != 0 {
		t.Errorf("audit count = %d, want 0 (self-reconnect, force не нужен)", n)
	}
}

// TestAcquireSoulLease_ForceRace_KeyChanged_FallbackNoHijack — гонка: prevKID
// доказанно мёртв, но между presence-чеком и force-release ключ сменился на
// третьего ЖИВОГО владельца (TTL истёк / другой keeper успел). CAS-by-prev-holder
// в ForceAcquireSoulLease вернёт ErrLeaseTaken → корректный fallback на
// AlreadyExists, без перехвата чужого свежего lease и без бесконечного цикла.
func TestAcquireSoulLease_ForceRace_KeyChanged_FallbackNoHijack(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	// prevKID мёртв в Conclave (presence не регистрируем) — но к моменту
	// force-release ключ уже принадлежит kid-fresh (эмуляция гонки: lease
	// перезаписан после того, как SoulLeaseOwner вернул prevKID). Чтобы
	// SoulLeaseOwner внутри handler-а вернул именно kid-dead, а CAS увидел
	// kid-fresh, подменяем seam owner-а на kid-dead, а реальный ключ ставим
	// в kid-fresh.
	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-fresh", 60*time.Second); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	h.soulLeaseOwner = func(context.Context, *keeperredis.Client, string) (string, bool, error) {
		return "kid-dead", true, nil
	}
	// kid-dead доказанно мёртв (его presence нет); kid-fresh жив — но force
	// CAS сверяет ключ с prevKID(kid-dead), не сматчит kid-fresh → ErrLeaseTaken.

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if cleanup != nil {
		t.Error("cleanup != nil, want nil (force провалился по гонке)")
	}
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (fallback после гонки)", got)
	}
	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-fresh" {
		t.Errorf("lease owner = %q, want kid-fresh (чужой свежий lease НЕ перехвачен)", owner)
	}
	if n := len(ca.snapshot()); n != 0 {
		t.Errorf("audit count = %d, want 0 (force не удался)", n)
	}
}

// TestAcquireSoulLease_NoConflict_HappyPath — нет конкурента: lease свободен →
// обычный захват, без presence-чека и без audit-а (контроль, что врезка не
// ломает штатный путь).
func TestAcquireSoulLease_NoConflict_HappyPath(t *testing.T) {
	h, rc, ca := newForceLeaseHandler(t, "kid-self")
	ctx := context.Background()
	sid := "host.example.com"

	cleanup, err := h.acquireSoulLease(ctx, sid)
	if err != nil {
		t.Fatalf("acquireSoulLease: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup = nil on happy path")
	}
	defer cleanup()

	if owner, _, _ := keeperredis.SoulLeaseOwner(ctx, rc, sid); owner != "kid-self" {
		t.Errorf("lease owner = %q, want kid-self", owner)
	}
	if n := len(ca.snapshot()); n != 0 {
		t.Errorf("audit count = %d, want 0 (нет force на свободном lease)", n)
	}
}
