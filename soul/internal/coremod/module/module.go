// Package module реализует core-модуль `core.module` ([ADR-065]) — доставку
// SoulModule-плагина на Soul-хост.
//
// Состояние:
//   - installed: плагин из активного Sigil-допуска стянут с Keeper-а
//     (FetchModule), верифицирован и атомарно установлен в каталожный слот
//     `<paths.modules>/<ns>-<name>/`. Идемпотентность — по sha256 бинаря
//     против активного допуска.
//
// Порядок apply-flow нормативен ([ADR-065](f)): allow-check ДО fetch →
// идемпотентность → fetch → verify ДО материализации → atomic rename →
// hot-register реестра custom-модулей ([Deps.Rescan], ADR-065(d)).
//
// [ADR-065]: docs/adr/0065-core-module-installed.md
package module

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — каноническая верхушка адреса.
const Name = "core.module"

const stateInstalled = "installed"

// reFullName — формат param name `<namespace>.<name>` (границы — naming-rules,
// «Каталог плагина»).
var reFullName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}\.[a-z][a-z0-9-]{0,62}$`)

// Fetcher — транспорт FetchModule ([ADR-012] третий RPC, ADR-065(a)).
// Реализуется soulgrpc.StreamSession; в прогон приходит через контекст
// ([WithFetcher]) — fetch привязан к живой EventStream-сессии, модуль stateless.
//
// [ADR-012]: docs/adr/0012-keeper-soul-grpc.md
type Fetcher interface {
	FetchModule(ctx context.Context, req *keeperv1.PluginFetchRequest) (grpc.ServerStreamingClient[keeperv1.PluginChunk], error)
}

type fetcherKey struct{}

// WithFetcher кладёт FetchModule-транспорт текущей сессии в ctx прогона
// (паттерн augur.WithRun: SoulModule-контракт state+params сессию не выражает).
func WithFetcher(ctx context.Context, f Fetcher) context.Context {
	if f == nil {
		return ctx
	}
	return context.WithValue(ctx, fetcherKey{}, f)
}

func fetcherFrom(ctx context.Context) (Fetcher, bool) {
	f, ok := ctx.Value(fetcherKey{}).(Fetcher)
	return f, ok
}

// Deps — host-зависимости модуля; wire-up в buildRegistry (cmd/soul).
// Zero-значение валидно и означает fail-closed: без Sigil-набора любой install
// отказывает module_not_allowed (push-режим `soul apply` без broadcast-кеша).
type Deps struct {
	// Sigils — активный локальный набор допусков (runtime-кеш sigilcache через
	// pluginhost-адаптер). nil = допусков нет.
	Sigils sharedhost.SigilLookup
	// Anchors — trust-anchor-ы подписи Sigil для verify скачанных байтов.
	Anchors *sharedhost.AnchorSet
	// ModulesRoot — корень кеша модулей Soul (paths.modules).
	ModulesRoot string
	// Rescan — hot-register реестра custom-модулей после успешной установки
	// (ADR-065(d)): установленный модуль доступен следующей задаче того же
	// прогона. nil = re-discover только на старте демона.
	Rescan func()
}

// Module — реализация sdk/module.SoulModule для core.module.
type Module struct {
	deps Deps
}

func New(deps Deps) *Module { return &Module{deps: deps} }

// Validate: known-state + required — из embed-манифеста; сверх него семантика,
// которую manifest-DSL не выражает — формат `<namespace>.<name>` у name.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)

	if util.ParamPresent(req.GetParams(), "name") {
		name, err := util.StringParam(req.GetParams(), "name")
		if err != nil {
			errs = append(errs, err.Error())
		} else if !reFullName.MatchString(name) {
			errs = append(errs, fmt.Sprintf("param %q: expected \"<namespace>.<name>\" (например community.redis), got %q", "name", name))
		}
	}
	if _, err := util.OptStringParam(req.GetParams(), "ref"); err != nil {
		errs = append(errs, err.Error())
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op (без PlanReadSafe): host применяет default-deny на dry_run.
// Pure-read drift потребовал бы allow-check+sha-сверку без побочек — отдельный
// слайс при реальном запросе.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.GetState() != stateInstalled {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q (want %s)", req.GetState(), stateInstalled))
	}
	return m.applyInstalled(stream, req)
}

// splitFullName разбирает `<namespace>.<name>` по первой точке.
func splitFullName(full string) (namespace, name string, ok bool) {
	if !reFullName.MatchString(full) {
		return "", "", false
	}
	namespace, name, ok = strings.Cut(full, ".")
	return namespace, name, ok
}
