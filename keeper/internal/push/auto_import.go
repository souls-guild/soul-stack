package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// AutoImportSystemAID — фиксированный AID, под которым auto-import пишет
// `created_by_aid` для `push_providers`-строк и `archon_aid` в audit-events.
// Не Архонт-оператор (нет JWT-инициатора), не [config.SourceKeeperInternal]
// (другая семантика — миграция данных по согласию оператора, не автономная
// инициатива Keeper-а). Отделено в системную AID для audit-фильтра.
//
// FK на `operators(aid)` для `push_providers.created_by_aid` обязывает строку
// `archon-system` существовать в реестре до первого auto-import-а. Строка
// `operators(archon-system, created_via='system', created_by_aid=NULL)` посеяна
// миграцией 086 (ADR-058(d)) — FK гарантирован после применения миграций.
const AutoImportSystemAID = "archon-system"

// AutoImporterReader — узкая read-поверхность над storage push_providers,
// нужная импортёру: проверка существования по PK перед INSERT. Симметрично
// [PushProviderResolver]; вынесено отдельным интерфейсом, чтобы unit-тест мог
// подменить fake без подъёма Postgres.
type AutoImporterReader interface {
	SelectByName(ctx context.Context, name string) (*pushprovider.PushProvider, error)
}

// AutoImporterInserter — узкая write-поверхность для INSERT push_providers
// при auto-import-е. Отдельно от [pushprovider.Service.Create]: импорт не
// должен публиковать `push-providers:changed` per-row и не пишет
// `push-provider.created` audit (вместо него — `push-provider.imported_from_config`
// с источником `config_bootstrap`).
type AutoImporterInserter interface {
	Insert(ctx context.Context, p *pushprovider.PushProvider) error
}

// AutoImporterTargetReader — узкая read-поверхность для проверки текущего
// `souls.ssh_target` (NULL → импортируем, не NULL → skip). Симметрично
// [PGTargetReader], но повторно объявлено, чтобы держать ровный per-importer
// набор сужений (на случай будущего разъезда).
type AutoImporterTargetReader interface {
	SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error)
}

// AutoImporterTargetWriter — узкая write-поверхность для UPDATE
// `souls.ssh_target` при auto-import-е.
type AutoImporterTargetWriter interface {
	UpdateSshTarget(ctx context.Context, sid string, target *soul.SSHTarget) error
}

// AuditWriter — узкое подмножество [audit.Writer]; повторно объявлено локально,
// чтобы фейк в unit-тесте не зависел от пакета audit.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// AutoImporterDeps — зависимости [AutoImporter].
//
// Все поля immutable после конструктора. Не использует *pgxpool.Pool напрямую —
// принимает узкие read/write-поверхности (read+write по targets, read+insert по
// providers), чтобы unit-тесты ходили через fake без подъёма PG.
type AutoImporterDeps struct {
	TargetReader   AutoImporterTargetReader
	TargetWriter   AutoImporterTargetWriter
	ProviderReader AutoImporterReader
	ProviderWriter AutoImporterInserter
	Auditor        AuditWriter
	Logger         *slog.Logger
}

// AutoImporter — one-shot миграция inline-`keeper.yml::push`-блоков в
// PG-источники (ADR-032 amendment 2026-05-26, S7-4).
//
// Запускается один раз в pipeline старта Keeper-а (см. wire-up в
// `runLegacyAutoImport` / `keeper/cmd/keeper`). Идемпотентно: записи,
// присутствующие в PG, пропускаются — повторный старт = no-op.
//
// Гейт — флаги `push.auto_import_legacy_targets` и
// `push.auto_import_legacy_providers` в [config.KeeperPush]. Default false:
// без явного opt-in импорт не выполняется (молчаливая миграция данных
// запрещена security policy).
type AutoImporter struct {
	deps AutoImporterDeps
}

// NewAutoImporter собирает импортёр. Любой nil в deps → ошибка конструктора
// (импортёр не имеет смысла без хотя бы одной полной пары: для targets — read
// + write, для providers — read + write; auditor + logger обязательны для
// аудит-trail-а).
func NewAutoImporter(deps AutoImporterDeps) (*AutoImporter, error) {
	if deps.TargetReader == nil || deps.TargetWriter == nil {
		return nil, errors.New("push: AutoImporter targets reader/writer is nil")
	}
	if deps.ProviderReader == nil || deps.ProviderWriter == nil {
		return nil, errors.New("push: AutoImporter providers reader/writer is nil")
	}
	if deps.Auditor == nil {
		return nil, errors.New("push: AutoImporter auditor is nil")
	}
	if deps.Logger == nil {
		return nil, errors.New("push: AutoImporter logger is nil")
	}
	return &AutoImporter{deps: deps}, nil
}

// ImportLegacyOnStart выполняет one-shot миграцию двух blocks по флагам
// [config.KeeperPush.AutoImportLegacyTargets] / `AutoImportLegacyProviders`.
//
// Контракт ошибок:
//   - PG-ошибка при чтении/записи любой записи прерывает импорт (fail-closed:
//     оператор включил auto-import, не должен молча получить «половина
//     импортирована, половина — нет»). Уже импортированные строки остаются;
//     неимпортированные — на следующем старте подхватятся (идемпотент);
//   - отсутствующая `souls`-row для config-target SID-а — WARN-skip (не fatal:
//     soul ещё не зарегистрирован, импорт SSH-реквизита бессмыслен — оператор
//     потом сам PUT-нёт через `/v1/souls/{sid}/ssh-target`);
//   - audit-write fail — лог WARN, продолжаем импорт (storage уже committed,
//     mismatch audit↔storage не блокирует следующие записи; паттерн bootstrap-а).
func (i *AutoImporter) ImportLegacyOnStart(ctx context.Context, cfg config.KeeperPush) error {
	if cfg.AutoImportLegacyTargets && len(cfg.Targets) > 0 {
		if err := i.importTargets(ctx, cfg.Targets); err != nil {
			return fmt.Errorf("push: auto-import legacy targets: %w", err)
		}
	}
	if cfg.AutoImportLegacyProviders && len(cfg.Providers) > 0 {
		if err := i.importProviders(ctx, cfg.Providers); err != nil {
			return fmt.Errorf("push: auto-import legacy providers: %w", err)
		}
	}
	return nil
}

func (i *AutoImporter) importTargets(ctx context.Context, targets []config.KeeperPushTarget) error {
	imported, skipped, missingSoul := 0, 0, 0

	for _, t := range targets {
		if t.SID == "" {
			// Schema-фаза уже отвергает такое; defense-in-depth.
			skipped++
			continue
		}
		existing, err := i.deps.TargetReader.SelectSshTarget(ctx, t.SID)
		if errors.Is(err, soul.ErrSoulNotFound) {
			i.deps.Logger.Warn("push: auto-import targets: souls row missing, skipping",
				slog.String("sid", t.SID))
			missingSoul++
			continue
		}
		if err != nil {
			return fmt.Errorf("read souls.ssh_target %q: %w", t.SID, err)
		}
		if existing != nil {
			// PG canonical-источник уже содержит данные — не перезаписываем
			// (идемпотент, повторный старт no-op).
			skipped++
			continue
		}

		// Pilot-форма S6 хранит только заданное оператором (резолверы
		// подставляют дефолты на пустых полях). Импорт сохраняет ту же
		// семантику: 0/"" пишем в jsonb как есть, дефолты подставит
		// PGFallbackTargetResolver при резолве.
		target := &soul.SSHTarget{
			SSHPort:  t.SSHPort,
			SSHUser:  t.SSHUser,
			SoulPath: t.SoulPath,
		}
		if err := i.deps.TargetWriter.UpdateSshTarget(ctx, t.SID, target); err != nil {
			return fmt.Errorf("update souls.ssh_target %q: %w", t.SID, err)
		}

		i.writeAudit(ctx, &audit.Event{
			EventType: audit.EventSoulSshTargetImportedFromConfig,
			Source:    audit.SourceConfigBootstrap,
			// archon_aid: NULL — system-action, не оператор-инициатор.
			Payload: map[string]any{
				"sid":       t.SID,
				"ssh_port":  t.SSHPort,
				"ssh_user":  t.SSHUser,
				"soul_path": t.SoulPath,
			},
		})
		imported++
	}

	i.deps.Logger.Info("push: S7-4 auto-import targets completed",
		slog.Int("imported", imported),
		slog.Int("skipped_existing", skipped),
		slog.Int("skipped_missing_soul", missingSoul),
		slog.Int("total_in_config", len(targets)))
	return nil
}

func (i *AutoImporter) importProviders(ctx context.Context, providers []config.KeeperPushProvider) error {
	imported, skipped := 0, 0

	for _, p := range providers {
		if p.Name == "" {
			skipped++
			continue
		}
		_, err := i.deps.ProviderReader.SelectByName(ctx, p.Name)
		if err == nil {
			// PG canonical-источник уже содержит запись — skip.
			skipped++
			continue
		}
		if !errors.Is(err, pushprovider.ErrPushProviderNotFound) {
			return fmt.Errorf("read push_providers %q: %w", p.Name, err)
		}

		entry := &pushprovider.PushProvider{
			Name:         p.Name,
			Params:       p.Params,
			CreatedByAID: AutoImportSystemAID,
		}
		if err := i.deps.ProviderWriter.Insert(ctx, entry); err != nil {
			// ErrPushProviderAlreadyExists на race здесь невозможен (старт
			// последовательный, один импортёр); пробрасываем как PG-ошибку.
			return fmt.Errorf("insert push_providers %q: %w", p.Name, err)
		}

		i.writeAudit(ctx, &audit.Event{
			EventType: audit.EventPushProviderImportedFromConfig,
			Source:    audit.SourceConfigBootstrap,
			Payload: map[string]any{
				"name":        p.Name,
				"params_keys": paramsKeys(p.Params),
			},
		})
		imported++
	}

	i.deps.Logger.Info("push: S7-4 auto-import providers completed",
		slog.Int("imported", imported),
		slog.Int("skipped_existing", skipped),
		slog.Int("total_in_config", len(providers)))
	return nil
}

// writeAudit — best-effort обёртка над auditor.Write. Storage уже committed,
// провал audit-write не должен прерывать импорт следующих записей (паттерн
// bootstrap.ErrAuditWriteFailed — расхождение разрешается оператором вручную).
func (i *AutoImporter) writeAudit(ctx context.Context, event *audit.Event) {
	if err := i.deps.Auditor.Write(ctx, event); err != nil {
		i.deps.Logger.Warn("push: S7-4 auto-import audit write failed",
			slog.String("event_type", string(event.EventType)),
			slog.Any("error", err))
	}
}

// paramsKeys возвращает отсортированный список ключей params без значений
// (симметрия с pushprovider.Service.Create payload: ключи фиксируются для
// аудит-trail-а, sensitive-значения по политике в audit не пишутся —
// устойчивость к будущему расширению allow-list).
func paramsKeys(params map[string]any) []string {
	if len(params) == 0 {
		return []string{}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PGTargetReadWriter / PGProviderReadWriter — конкретные адаптеры
// pgxpool.Pool (или любой соответствующий ExecQueryRower) к интерфейсам
// AutoImporterTargetReader+Writer / AutoImporterReader+Inserter. Live тонкая
// обёртка; нужна, чтобы daemon-wire-up не строил два отдельных адаптера
// (read+write одна и та же *pgxpool.Pool) и не светил pgx-deps в Auto-Importer.

// pgTargetReadWriter — продакшен-implementation для targets.
type pgTargetReadWriter struct {
	db soul.ExecQueryRower
}

// NewPGTargetReadWriter возвращает реализацию AutoImporterTargetReader +
// AutoImporterTargetWriter поверх soul.ExecQueryRower (тип *pgxpool.Pool).
func NewPGTargetReadWriter(db soul.ExecQueryRower) interface {
	AutoImporterTargetReader
	AutoImporterTargetWriter
} {
	return &pgTargetReadWriter{db: db}
}

func (a *pgTargetReadWriter) SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error) {
	return soul.SelectSshTarget(ctx, a.db, sid)
}

func (a *pgTargetReadWriter) UpdateSshTarget(ctx context.Context, sid string, target *soul.SSHTarget) error {
	return soul.UpdateSshTarget(ctx, a.db, sid, target)
}

// pgProviderReadWriter — продакшен-implementation для providers.
type pgProviderReadWriter struct {
	db pushprovider.ExecQueryRower
}

// NewPGProviderReadWriter возвращает реализацию AutoImporterReader +
// AutoImporterInserter поверх pushprovider.ExecQueryRower (*pgxpool.Pool).
func NewPGProviderReadWriter(db pushprovider.ExecQueryRower) interface {
	AutoImporterReader
	AutoImporterInserter
} {
	return &pgProviderReadWriter{db: db}
}

func (a *pgProviderReadWriter) SelectByName(ctx context.Context, name string) (*pushprovider.PushProvider, error) {
	return pushprovider.SelectByName(ctx, a.db, name)
}

func (a *pgProviderReadWriter) Insert(ctx context.Context, p *pushprovider.PushProvider) error {
	return pushprovider.Insert(ctx, a.db, p)
}
