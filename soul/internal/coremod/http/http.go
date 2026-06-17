// Package http реализует core-модуль `core.http` ([ADR-015]) — read-probe
// HTTP-эндпоинта (health-check / API-readiness / чтение версии). Идея
// заимствована из Ansible `uri`, но сознательно сужена до чтения: «делаем
// хорошо» вместо мутной вседозволенности.
//
// Verb MVP:
//   - probe: GET/HEAD-запрос к url, ответ возвращается в register
//     (status / body / elapsed_ms / headers_keys). Состояние хоста НЕ меняется
//     (см. ниже), поэтому это verb-форма, а не declarative-state.
//
// Семантика changed:
//   - changed = false ВСЕГДА, конструктивно и ненастраиваемо: read-probe не
//     меняет состояние хоста. Прецедент — `core.exec.run` (модуль даёт факты,
//     а интерпретирует их `changed_when:` на уровне scenario).
//
// Идемпотентность: read-probe идемпотентен по природе (no-op для состояния).
//
// Безопасность ([ADR-016] «безопасность на первом месте»). secure-by-default:
// все три контура взведены, каждый снимается отдельным явным opt-out-param-ом
// (ортогонально — снятие одного не ослабляет другие):
//   - https-only (default): http:// и file:// отвергаются
//     (util.ValidateFetchURL — https при default, http(s) при allow_http).
//     Снять до http(s) — `allow_http: true` (file:// остаётся запрещён);
//   - SSRF-guard (default): probe в metadata/loopback/RFC1918/link-local
//     заблокирован по фактически резолвнутому IP (закрывает прямой SSRF на
//     cloud-metadata IAM 169.254.169.254 и DNS-rebind, см. util.NewHTTPClient).
//     Снять для легитимного internal health-check — `allow_private: true`;
//   - TLS-верификация (default): системный trust store. Снять для self-signed/
//     internal CA — `insecure_skip_verify: true` (MITM-риск);
//   - редирект на не-https блокируется (util.CheckRedirect, downgrade-защита);
//     при allow_http downgrade-hop https→http допускается (AllowHTTPRedirect);
//   - headers — sensitive-by-construction ([ADR-010] §7.4): значения никогда
//     не логируются и не попадают в output (в output отдаётся только список
//     ключей запрошенных заголовков).
//
// При снятии любого guard probe возвращает warning в output (поле warnings,
// конвенция core.repo/core.url): оператор видит факт ослабления контура. В
// warning попадает только host (НЕ полный URL и НЕ headers — sensitive).
//
// Мутирующие HTTP (POST/PUT/PATCH/DELETE) сознательно отложены post-MVP
// отдельным ADR-расширением (вероятно `core.http.request`) — тогда же решается
// changed-контракт для мутаций. Verb `probe` остаётся строго read-only.
//
// [ADR-010]: docs/adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов
// [ADR-015]: docs/adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список
// [ADR-016]: docs/adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack
package http

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name — каноническая верхушка адреса.
const Name = "core.http"

// defaultTimeout — таймаут probe по умолчанию, если param timeout не задан.
// Короче, чем у core.url (300s): probe — health-check, а не download.
const defaultTimeout = 30 * time.Second

// defaultMethod — HTTP-метод по умолчанию. Только GET/HEAD (read-only).
const defaultMethod = http.MethodGet

// maxBodyBytes — жёсткий cap на читаемое тело ответа (защита от OOM на большом
// ответе). Тело сверх лимита отбрасывается, в output ставится truncated=true.
const maxBodyBytes = 64 * 1024

// allowedMethods — read-only методы, разрешённые verb-ом probe. Мутирующие
// (POST/PUT/PATCH/DELETE) сознательно отсутствуют — см. doc-комментарий пакета.
var allowedMethods = map[string]struct{}{
	http.MethodGet:  {},
	http.MethodHead: {},
}

// Module — реализация sdk/module.SoulModule для core.http.
//
// HTTP-клиент строится per-call фабрикой NewClient из набора opt-out-флагов
// (allow_private / allow_http / insecure_skip_verify). Три ортогональных bool =
// 2³=8 комбинаций — пред-собранные инстанции клиента не масштабируются, поэтому
// клиент собирается just-in-time под фактические флаги задачи.
//
// NewClient вынесен в поле для подмены в unit-тестах: тесты подставляют фабрику,
// возвращающую fake HTTPDoer без выхода в сеть (и могут проверить, с какими
// HTTPClientOpts модуль её вызвал).
type Module struct {
	// NewClient — фабрика HTTP-клиента под opt-out-флаги задачи. В проде —
	// util.NewHTTPClient (системный TLS trust store, downgrade-защита редиректов,
	// SSRF-guard на dial-фазе; каждый контур ослабляется отдельным полем opts).
	// В тестах подменяется на возврат fake HTTPDoer.
	NewClient func(util.HTTPClientOpts) util.HTTPDoer
}

func New() *Module {
	return &Module{
		NewClient: func(opts util.HTTPClientOpts) util.HTTPDoer { return util.NewHTTPClient(opts) },
	}
}

// Validate НЕ делегирован целиком в util.ValidateAgainstManifest (в отличие от
// core.exec): сверх known-state + required у core.http есть семантические
// проверки, которые manifest-DSL не выражает — схема URL (ValidateFetchURL,
// https-only при default, http(s) при allow_http), enum method (GET|HEAD), парс
// timeout-duration. Они критичны (ADR-016: SSRF/http-downgrade/мутирующие методы
// отвергаются на Validate). тип-чек bool-флагов (allow_private/allow_http/
// insecure_skip_verify) тоже на Validate, чтобы кривой тип падал до Apply.
// known-state/required
// дублируются с http.yaml осознанно — единый источник невозможен без этих
// семантик в DSL.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "probe" {
		errs = append(errs, fmt.Sprintf("unknown verb %q (want probe)", req.State))
	}

	// allow_http проверяется до url: его значение определяет, какую схему
	// принимает ValidateFetchURL (https-only при false, http(s) при true).
	allowHTTP, berr := util.OptBoolParam(req.Params, "allow_http")
	if berr != nil {
		errs = append(errs, berr.Error())
	}

	rawURL, err := util.StringParam(req.Params, "url")
	if err != nil {
		errs = append(errs, err.Error())
	} else if serr := util.ValidateFetchURL(rawURL, allowHTTP); serr != nil {
		errs = append(errs, serr.Error())
	}

	if _, merr := normalizedMethod(req.Params); merr != nil {
		errs = append(errs, merr.Error())
	}

	if _, serr := util.OptIntSliceParam(req.Params, "status_codes"); serr != nil {
		errs = append(errs, serr.Error())
	}

	if _, berr := util.OptBoolParam(req.Params, "allow_private"); berr != nil {
		errs = append(errs, berr.Error())
	}

	if _, berr := util.OptBoolParam(req.Params, "insecure_skip_verify"); berr != nil {
		errs = append(errs, berr.Error())
	}

	if ts, terr := util.OptStringParam(req.Params, "timeout"); terr != nil {
		errs = append(errs, terr.Error())
	} else if ts != "" {
		if _, derr := parseTimeout(ts); derr != nil {
			errs = append(errs, derr.Error())
		}
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op (без PlanReadSafe). core.http — verb-модуль (probe): read-probe
// HTTP-эндпоинта, у него НЕТ желаемого состояния хоста, сверяемого pure-read-ом
// (changed всегда false конструктивно, см. doc пакета). Drift в смысле ADR-031
// не определён. Host применяет default-deny: dry_run для core.http возвращает
// FAILED `plan.unsupported`, и это конструктивный отказ — НЕ ложное «нет
// дрифта». Сам probe — read-only по природе, но вне контракта Plan/Apply ADR-031.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// ErrandReadSafe — marker [sdkmodule.ErrandReadSafe] (ADR-033 §2): probe — это
// read-only HTTP-запрос, не мутирующий состояние хоста (`changed = false`
// конструктивно, см. doc пакета). Безопасен к ad-hoc invocation через Errand,
// поэтому модуль явно опт-инит в whitelist Errand-runner-а. Verb-модули
// core.cmd.shell / core.exec.run остаются в hardcoded-списке (императивны
// by-design), здесь декларация для будущих read-safe core-добавлений и
// симметрии с интерфейсным контрактом sdk/module.
func (m *Module) ErrandReadSafe() {}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != "probe" {
		return util.SendFailed(stream, fmt.Sprintf("unknown verb %q", req.State))
	}
	return m.applyProbe(stream, req)
}

// normalizedMethod возвращает HTTP-метод probe: пустой param → defaultMethod;
// иначе — значение, проверенное по allowedMethods. Возвращает ошибку для
// неизвестного/мутирующего метода. Сравнение по верхнему регистру (get → GET).
func normalizedMethod(params *structpb.Struct) (string, error) {
	raw, err := util.OptStringParam(params, "method")
	if err != nil {
		return "", err
	}
	if raw == "" {
		return defaultMethod, nil
	}
	m := strings.ToUpper(raw)
	if _, ok := allowedMethods[m]; !ok {
		return "", fmt.Errorf("param %q: unsupported method %q (want GET|HEAD)", "method", raw)
	}
	return m, nil
}

// parseTimeout разбирает param timeout по convention `duration` Soul Stack
// (Go time.ParseDuration + суффикс `<N>d`), через единый shared/config-парсер
// (симметрично core.url).
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
