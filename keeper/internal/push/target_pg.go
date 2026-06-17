package push

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// PGTargetReader — узкая поверхность над storage-слоем soul.* для PG-резолва
// ssh_target по SID. Сужено до одного метода, чтобы unit-test PGFallbackTargetResolver
// мог подставить fake без подъёма Postgres-pool-а (паттерн [SoulLookup]).
//
// Production wire-up — обёртка через [NewPGTargetReader] поверх pgxpool.Pool.
type PGTargetReader interface {
	SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error)
}

// pgPoolTargetReader — production-implementation [PGTargetReader] поверх
// pgxpool.Pool / соответствующего soul.ExecQueryRower-handle.
type pgPoolTargetReader struct {
	db soul.ExecQueryRower
}

// NewPGTargetReader адаптирует pgxpool.Pool (или любой soul.ExecQueryRower) под
// [PGTargetReader]. Используется setupPushDispatchers в daemon-wire-up.
func NewPGTargetReader(db soul.ExecQueryRower) PGTargetReader {
	return &pgPoolTargetReader{db: db}
}

func (r *pgPoolTargetReader) SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error) {
	return soul.SelectSshTarget(ctx, r.db, sid)
}

// PGFallbackTargetResolver — PG-first резолвер SSH-реквизитов с опциональным
// fallback на keeper.yml::push.targets[] (ADR-032 amendment 2026-05-26, S7-1).
//
// Алгоритм Resolve:
//
//  1. SELECT souls.ssh_target по SID:
//     - row не найден → пробрасываем soul.ErrSoulNotFound (звучит как
//     инвариант-нарушение: SshDispatcher уже резолвил Soul-row до этой
//     точки в SendApply, но defensive — guard);
//     - ssh_target IS NULL → переход к шагу 2;
//     - ssh_target проставлен → собираем [SSHTarget] с подстановкой дефолтов
//     (port 22 / user root / soul-path /usr/local/bin/soul) для опущенных
//     полей. Возврат.
//
//  2. AllowLegacy=false (default S7-1) → возвращаем `ErrTargetNotConfigured`.
//     AllowLegacy=true → одноразовый WARN deprecation-log + делегируем в
//     [Fallback] (ConfigTargetResolver поверх keeper.yml::push.targets[]).
//
// Семантика sentinel-ошибок такая же, как у [ConfigTargetResolver]:
// `ErrTargetNotConfigured` отделяет «оператор не настроил» от транспортных
// ошибок.
type PGFallbackTargetResolver struct {
	Reader       PGTargetReader
	Fallback     TargetResolver
	AllowLegacy  bool
	Logger       *slog.Logger
	legacyWarned sync.Once
}

// Resolve реализует [TargetResolver].
func (r *PGFallbackTargetResolver) Resolve(ctx context.Context, sid string) (SSHTarget, error) {
	target, err := r.Reader.SelectSshTarget(ctx, sid)
	if err != nil {
		// soul.ErrSoulNotFound — нештатный путь (SshDispatcher уже валидировал
		// soul-row до Resolve), но пробрасываем как fail-closed, чтобы caller
		// видел чёткое сообщение в push_runs.summary.
		return SSHTarget{}, fmt.Errorf("push: read ssh_target %s: %w", sid, err)
	}
	if target != nil {
		return SSHTarget{
			Host:     sid,
			Port:     resolveInt(target.SSHPort, defaultSSHPort),
			User:     resolveStr(target.SSHUser, defaultSSHUser),
			SoulPath: resolveStr(target.SoulPath, defaultSoulPath),
		}, nil
	}

	// PG-row.ssh_target IS NULL: переключаемся на legacy-fallback, если оператор
	// явно разрешил его флагом `push.allow_legacy_push_targets: true`.
	if !r.AllowLegacy || r.Fallback == nil {
		return SSHTarget{}, fmt.Errorf("%w: %s", ErrTargetNotConfigured, sid)
	}

	r.legacyWarned.Do(func() {
		if r.Logger != nil {
			r.Logger.Warn("push: S7-1 deprecation: keeper.yml::push.targets[] используется как fallback; мигрируйте на souls.ssh_target через PUT /v1/souls/{sid}/ssh-target",
				slog.String("trigger_sid", sid))
		}
	})
	return r.Fallback.Resolve(ctx, sid)
}
