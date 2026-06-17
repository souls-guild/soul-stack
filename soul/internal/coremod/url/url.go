// Package url реализует core-модуль `core.url` ([ADR-015]) — загрузку файла
// по URL (аналог Ansible get_url).
//
// Состояние:
//   - fetched: по адресу url скачивается файл в path. Идемпотентность — через
//     checksum (если задан) или через сравнение SHA-256 содержимого.
//
// Безопасность ([ADR-016] «безопасность на первом месте», secure-by-default +
// явный opt-out): по умолчанию максимально строгий режим, оператор снимает
// каждый контур отдельным флагом:
//   - только https:// — http:// и file:// отвергаются в Validate (защита от
//     downgrade и чтения локальной ФС); снимается флагом allow_http (допускает
//     http://, но НЕ открывает SSRF — dial-guard держится отдельно);
//   - SSRF-guard: dial в metadata/loopback/RFC1918/link-local заблокирован по
//     фактически резолвнутому IP (закрывает прямой SSRF и DNS-rebind); снимается
//     флагом allow_private (легитимный internal endpoint), см. util.NewHTTPClient;
//   - TLS — системный trust store по умолчанию; проверка цепочки снимается
//     флагом insecure_skip_verify (self-signed / internal CA, MITM-риск);
//   - checksum verify происходит по временному файлу ДО публикации в path:
//     неверный хэш никогда не материализуется (supply-chain);
//   - headers — sensitive-by-construction ([ADR-010] §7.4): значения никогда
//     не логируются и не попадают в output/register;
//   - снятие любого guard-флага сопровождается warning в output ApplyEvent (поле
//     warnings, конвенция core.repo/core.http): оператор видит факт ослабления
//     контура в RunResult. В warning попадает только host (без полного URL и
//     headers — могут нести секреты).
//
// Три флага ортогональны: allow_http ослабляет ТОЛЬКО проверку схемы,
// allow_private — ТОЛЬКО dial-guard, insecure_skip_verify — ТОЛЬКО TLS-цепочку.
//
// [ADR-010]: docs/adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов
// [ADR-015]: docs/adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список
// [ADR-016]: docs/adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack
package url

import (
	"context"
	"fmt"
	"os/user"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — каноническая верхушка адреса.
const Name = "core.url"

// defaultTimeout — таймаут запроса по умолчанию, если param timeout не задан.
const defaultTimeout = 300 * time.Second

// Module — реализация sdk/module.SoulModule для core.url.
//
// NewClient / Lookup{User,Group} вынесены в поля для подмены в unit-тестах
// (образец — util.Runner / LookupUser-инъекция в соседних модулях; единый
// test-seam NewClient — симметрично core.http).
type Module struct {
	// NewClient строит HTTP-клиент под per-Apply набор opt-out-флагов
	// (allow_http / allow_private / insecure_skip_verify). В проде —
	// util.NewHTTPClient (дефолт New()); в тестах подменяется на конструктор
	// fake-клиента. Каждый Apply строит клиент заново из распарсенных флагов:
	// флаги одной задачи не должны протекать в другую.
	NewClient func(util.HTTPClientOpts) util.HTTPDoer
	// LookupUser / LookupGroup — точки подмены для unit-тестов owner/group.
	LookupUser  func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)
}

func New() *Module {
	return &Module{
		// Дефолтная фабрика: системный trust store, downgrade-защита редиректов,
		// SSRF-guard — всё определяется переданными флагами (нулевой opts =
		// максимально безопасный клиент, см. util.NewHTTPClient).
		NewClient: func(opts util.HTTPClientOpts) util.HTTPDoer {
			return util.NewHTTPClient(opts)
		},
		LookupUser:  user.Lookup,
		LookupGroup: user.LookupGroup,
	}
}

// Validate НЕ делегирован целиком в util.ValidateAgainstManifest (в отличие от
// core.exec): сверх known-state + required у core.url есть семантические проверки,
// которые manifest-DSL не выражает — схема URL (util.ValidateFetchURL: https при
// default, http(s) при allow_http), форма checksum
// ("sha256:<hex>"), парс timeout-duration. Они критичны (ADR-016 «безопасность
// на первом месте»: http-downgrade/SSRF отвергается на Validate). known-state/
// required дублируются с url.yaml осознанно — единый источник невозможен без
// этих семантик в DSL.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "fetched" {
		errs = append(errs, fmt.Sprintf("unknown state %q (want fetched)", req.State))
	}

	// opt-out-флаги тип-чекаются здесь же; allow_http влияет на допустимость
	// схемы url (https-only vs http(s)), поэтому парсится до проверки url.
	allowHTTP, err := util.OptBoolParam(req.Params, "allow_http")
	if err != nil {
		errs = append(errs, err.Error())
	}
	if _, berr := util.OptBoolParam(req.Params, "insecure_skip_verify"); berr != nil {
		errs = append(errs, berr.Error())
	}
	if _, berr := util.OptBoolParam(req.Params, "allow_private"); berr != nil {
		errs = append(errs, berr.Error())
	}

	rawURL, err := util.StringParam(req.Params, "url")
	if err != nil {
		errs = append(errs, err.Error())
	} else if serr := util.ValidateFetchURL(rawURL, allowHTTP); serr != nil {
		errs = append(errs, serr.Error())
	}

	if _, err := util.StringParam(req.Params, "path"); err != nil {
		errs = append(errs, err.Error())
	}

	if cs, err := util.OptStringParam(req.Params, "checksum"); err != nil {
		errs = append(errs, err.Error())
	} else if cs != "" {
		if _, _, cerr := parseChecksum(cs); cerr != nil {
			errs = append(errs, cerr.Error())
		}
	}

	if ts, err := util.OptStringParam(req.Params, "timeout"); err != nil {
		errs = append(errs, err.Error())
	} else if ts != "" {
		if _, derr := parseTimeout(ts); derr != nil {
			errs = append(errs, derr.Error())
		}
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op (без PlanReadSafe). core.url НЕ объявляет read-safe Plan в MVP,
// host применяет default-deny на dry_run (FAILED `plan.unsupported`). Причина:
// pure-read drift «нужно ли скачать?» для бесчексумной ветки требует HEAD-
// запроса к remote-у (или GET), которого Apply ДО мутации НЕ делает (Apply
// сразу GET-ит во temp). Чексумная ветка теоретически выводима из существующего
// read (sha256 локального файла + сравнение с checksum), но реализовывать
// половину контракта означало бы непредсказуемый dry_run (зависит от наличия
// checksum-а у конкретной задачи). Целостный pure-read путь — отдельный slice:
// либо HEAD-probe с opt-out-флагами симметрично Apply, либо явный split
// «по checksum-у». Пока — default-deny.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != "fetched" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	return m.applyFetched(stream, req)
}

// parseTimeout разбирает param timeout по convention `duration` Soul Stack
// (Go time.ParseDuration + суффикс `<N>d`), через единый shared/config-парсер.
func parseTimeout(s string) (time.Duration, error) {
	d, err := config.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("param %q: invalid duration %q", "timeout", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("param %q: must be positive, got %q", "timeout", s)
	}
	return d, nil
}
