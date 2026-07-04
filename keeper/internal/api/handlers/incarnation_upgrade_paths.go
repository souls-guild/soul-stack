package handlers

// GET /v1/incarnations/{name}/upgrade-paths (ADR-0068 §6) — READ-анализ путей
// апгрейда инкарнации. Отдельный файл (не incarnation_typed.go), чтобы не
// конфликтовать с upgrade-флоу Slice 2. Read-only переиспользование строительных
// блоков incarnation.PrepareUpgrade (резолв+загрузка+анализ) БЕЗ смены пина/запуска.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
)

// Направления перехода (ADR-0068 §6, on-demand анализ одной цели).
const (
	upgradeDirectionNoop       = "no-op"       // to==пин И схема цели==текущая
	upgradeDirectionDowngrade  = "downgrade"   // схема цели < текущей (forward-only, ADR-019)
	upgradeDirectionForward    = "forward"     // схема цели > текущей
	upgradeDirectionSameSchema = "same-schema" // схемы равны, ref другой (ref-bump)
)

// Режимы наличия upgrade-сценария для перехода (ADR-0068 §5).
const (
	upgradeModeFound  = "found"  // есть upgrade-сценарий, чей from ⊇ текущего пина
	upgradeModeLegacy = "legacy" // нет — апгрейд ушёл бы в drift без host-оркестрации
)

// IncarnationUpgradePathsView — ПЛОСКАЯ доменная проекция GET .../upgrade-paths.
// Два режима одного эндпоинта (ADR-0068 §6):
//   - дешёвый (toRef==""): заполнен Paths — теги реестра сервиса + пометка is_current;
//     направление/found НЕ вычисляем (ADR-007 запрет semver-парсинга — по именам тегов
//     направление недостоверно).
//   - on-demand (toRef!=""): заполнен Target — анализ ОДНОЙ цели (direction/mode/slug/
//     state_migrations).
type IncarnationUpgradePathsView struct {
	CurrentVersion            string                 // inc.ServiceVersion (текущий пин)
	CurrentStateSchemaVersion int                    // inc.StateSchemaVersion
	Paths                     []UpgradePathRefView   // дешёвый режим (nil в on-demand)
	Target                    *UpgradePathTargetView // on-demand (nil в дешёвом)
}

// UpgradePathRefView — один git-ref реестра сервиса (дешёвый режим). IsCurrent —
// no-op-пометка ref == inc.ServiceVersion (ADR-0068 §6).
type UpgradePathRefView struct {
	Ref       string
	Type      string // tag | branch (artifact.GitRef.Type)
	Commit    string
	IsCurrent bool
}

// UpgradePathTargetView — read-only анализ одной цели (on-demand). Срез
// incarnation.PrepareUpgrade без смены пина/запуска (ADR-0068 §6).
type UpgradePathTargetView struct {
	To                       string
	ResolvedCommit           string // art.SHA1 снапшота цели
	TargetStateSchemaVersion int
	Direction                string // no-op | downgrade | forward | same-schema
	Mode                     string // found | legacy
	Slug                     string // slug upgrade-сценария при found
	Downgrade                bool   // цель ниже по схеме → цепочку НЕ грузим (forward-only)
	// Reachable — цель достижима апгрейдом. false ТОЛЬКО при структурно битой цепочке
	// миграций (unreachable_reason); downgrade/no-op — reachable=true (это другое
	// направление, сигналится Direction, а не «недостижимость»).
	Reachable bool
	// UnreachableReason — машинная причина недостижимости (пусто при reachable=true).
	UnreachableReason string
	// StateMigrations — применяемая цепочка current→target (форма {from,to,path}).
	// При downgrade / битой цепочке — пусто (не грузим / собрать нельзя).
	StateMigrations []artifact.Migration
}

// UpgradePathsTyped — GET /v1/incarnations/{name}/upgrade-paths (READ, без audit,
// ADR-0068 §6). toRef=="" → дешёвый список тегов; toRef!="" → on-demand анализ цели.
// inScope — scope-предикат оператора (как GetTyped): вне scope → 404 (не палим
// существование). Read-only: пин НЕ меняется, апгрейд НЕ выполняется.
func (h *IncarnationHandler) UpgradePathsTyped(ctx context.Context, name, toRef string, inScope func(*incarnation.Incarnation) bool) (IncarnationUpgradePathsView, error) {
	var zero IncarnationUpgradePathsView
	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	// existence-probe + scope: not-found / вне scope → 404 (не палим существование).
	inc, err := h.existenceProbeInScope(ctx, name, inScope, "upgrade-paths")
	if err != nil {
		return zero, err
	}

	view := IncarnationUpgradePathsView{
		CurrentVersion:            inc.ServiceVersion,
		CurrentStateSchemaVersion: inc.StateSchemaVersion,
	}
	if toRef == "" {
		paths, perr := h.upgradePathsCheap(ctx, inc)
		if perr != nil {
			return zero, perr
		}
		view.Paths = paths
		return view, nil
	}
	target, terr := h.upgradePathsTarget(ctx, inc, toRef)
	if terr != nil {
		return zero, terr
	}
	view.Target = target
	return view, nil
}

// upgradePathsCheap — дешёвый режим (ADR-0068 §6): ls-remote тегов реестра сервиса +
// пометка is_current. Тот же источник, что ServiceHandler.ListRefsTyped (h.services.
// Resolve → git-координаты, h.refs.ListRefs → теги). Направление/found НЕ вычисляем
// (ADR-007 запрет semver-парсинга; недостоверно по именам тегов) — это on-demand ?to=.
func (h *IncarnationHandler) upgradePathsCheap(ctx context.Context, inc *incarnation.Incarnation) ([]UpgradePathRefView, error) {
	if h.refs == nil {
		return nil, incProblem(problem.TypeInternalError, "service refs lister is not configured")
	}
	if h.services == nil {
		return nil, incProblem(problem.TypeInternalError, "service resolver is not configured")
	}
	ref, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil, incProblem(problem.TypeNotFound, "service "+inc.Service+" is not registered")
	}
	refs, err := h.refs.ListRefs(ctx, ref.Name, ref.Git)
	if err != nil {
		h.logger.Warn("incarnation.upgrade-paths: ls-remote failed",
			slog.String("name", inc.Name), slog.String("service", inc.Service), slog.Any("error", err))
		return nil, incProblem(problem.TypeBadGateway, "ls-remote failed for service "+inc.Service+": "+err.Error())
	}
	out := make([]UpgradePathRefView, 0, len(refs))
	for _, r := range refs {
		out = append(out, UpgradePathRefView{
			Ref:       r.Name,
			Type:      r.Type,
			Commit:    r.Commit,
			IsCurrent: r.Name == inc.ServiceVersion,
		})
	}
	return out, nil
}

// upgradePathsTarget — on-demand анализ ОДНОЙ цели (ADR-0068 §6): загрузка снапшота
// toRef → direction / mode(found|legacy) / state_migrations. Read-only срез
// incarnation.PrepareUpgrade — пин НЕ меняется, апгрейд НЕ выполняется.
func (h *IncarnationHandler) upgradePathsTarget(ctx context.Context, inc *incarnation.Incarnation, toRef string) (*UpgradePathTargetView, error) {
	if h.services == nil || h.loader == nil {
		return nil, incProblem(problem.TypeInternalError, "service loader is not configured")
	}
	ref, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil, incProblem(problem.TypeNotFound, "service "+inc.Service+" is not registered")
	}
	ref.Ref = toRef
	art, err := h.loader.Load(ctx, ref)
	if err != nil {
		h.logger.Warn("incarnation.upgrade-paths: load target snapshot failed",
			slog.String("name", inc.Name), slog.String("to", toRef), slog.Any("error", err))
		return nil, incProblem(problem.TypeBadGateway, "load target snapshot "+toRef+" failed: "+err.Error())
	}
	if art == nil || art.Manifest == nil {
		return nil, incProblem(problem.TypeInternalError, "target snapshot "+toRef+" has no manifest")
	}
	target := art.Manifest.StateSchemaVersion
	current := inc.StateSchemaVersion

	tgt := &UpgradePathTargetView{
		To:                       toRef,
		ResolvedCommit:           art.SHA1,
		TargetStateSchemaVersion: target,
		Reachable:                true, // сбрасывается в false только на битой цепочке
	}
	switch {
	case target < current:
		tgt.Direction = upgradeDirectionDowngrade
		tgt.Downgrade = true
	case target > current:
		tgt.Direction = upgradeDirectionForward
	case toRef == inc.ServiceVersion:
		tgt.Direction = upgradeDirectionNoop
	default:
		tgt.Direction = upgradeDirectionSameSchema
	}

	// mode found/legacy — ТОЛЬКО для апгрейд-направлений (forward/same-schema): при
	// downgrade/no-op семантически бессмыслен, ListUpgrades не дёргаем. Скан upgrade/
	// цели, матч from ⊇ текущего пина (ResolveUpgradeScenario). Сбой скана уводим в
	// legacy (ADR-0068 §5★ fail-open): upgrade-paths честно показывает, ЧТО произойдёт.
	if tgt.Direction == upgradeDirectionForward || tgt.Direction == upgradeDirectionSameSchema {
		upgrades, uerr := h.loader.ListUpgrades(art)
		if uerr != nil {
			h.logger.Warn("incarnation.upgrade-paths: upgrade scan failed, reporting legacy",
				slog.String("name", inc.Name), slog.String("to", toRef), slog.Any("error", uerr))
		}
		if slug, found := artifact.ResolveUpgradeScenario(upgrades, inc.ServiceVersion); found {
			tgt.Mode = upgradeModeFound
			tgt.Slug = slug
		} else {
			tgt.Mode = upgradeModeLegacy
		}
	}

	// state_migrations — применяемая цепочка current→target. При downgrade НЕ грузим
	// (forward-only, ADR-019; LoadMigrationChain на from>to вернул бы ошибку).
	if !tgt.Downgrade {
		chain, cerr := h.loader.LoadMigrationChain(art, current, target)
		if cerr != nil {
			if errors.Is(cerr, artifact.ErrMigrationChainBroken) {
				// Preview-эндпоинт (ADR-0068 §6): структурно битая цепочка — недостижимая
				// цель как ДАННЫЕ, НЕ HTTP-ошибка. direction/mode уже вычислены (forward,
				// found/legacy), цепочку собрать нельзя → reachable=false + причина,
				// state_migrations пуст. Оператор видит «сюда перейти нельзя», не 4xx.
				h.logger.Warn("incarnation.upgrade-paths: target unreachable — migration chain broken",
					slog.String("name", inc.Name), slog.String("to", toRef), slog.Any("error", cerr))
				tgt.Reachable = false
				tgt.UnreachableReason = "migration chain to " + toRef + " is broken: " + cerr.Error()
				return tgt, nil
			}
			// Прочий сбой (парс кривого migrations/-файла / I/O уже материализованного
			// снапшота) = keeper-internal дефект → 500 (parity UpgradeTyped default). 502
			// оставлен ТОЛЬКО за loader.Load — там виновник реально внешний git.
			h.logger.Error("incarnation.upgrade-paths: load migration chain failed",
				slog.String("name", inc.Name), slog.String("to", toRef), slog.Any("error", cerr))
			return nil, incProblem(problem.TypeInternalError, "load migration chain to "+toRef+" failed")
		}
		tgt.StateMigrations = upgradeMigrationSteps(chain)
	}
	return tgt, nil
}

// upgradeMigrationSteps проецирует применяемую цепочку в {from,to,path}-форму
// ([artifact.Migration]). Path — каноническое имя файла миграции (docs/migrations.md,
// migrations/<NNN>_to_<MMM>.yml) из собственных версий шага (display-путь, не
// дублирование логики LoadMigrationChain).
func upgradeMigrationSteps(chain statemigrate.Chain) []artifact.Migration {
	steps := make([]artifact.Migration, 0, len(chain))
	for _, m := range chain {
		steps = append(steps, artifact.Migration{
			From: m.FromVersion,
			To:   m.ToVersion,
			Path: fmt.Sprintf("migrations/%03d_to_%03d.yml", m.FromVersion, m.ToVersion),
		})
	}
	return steps
}
