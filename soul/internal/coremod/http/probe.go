package http

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// applyProbe реализует verb `probe`: один GET/HEAD-запрос к url, ответ —
// в register. Состояние хоста не меняется → changed=false всегда.
//
// Контракт ошибок:
//   - транспортная ошибка (DNS/TLS/timeout/заблокированный downgrade-редирект)
//     → failed (output бессмысленен);
//   - статус-код вне status_codes (default [200]) → failed, но с output:
//     оператору нужны фактический status/body для диагностики.
func (m *Module) applyProbe(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest) error {
	rawURL, err := util.StringParam(req.Params, "url")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	allowHTTP, err := util.OptBoolParam(req.Params, "allow_http")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if verr := util.ValidateFetchURL(rawURL, allowHTTP); verr != nil {
		return util.SendFailed(stream, verr.Error())
	}
	method, err := normalizedMethod(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	headers, err := util.OptStringMapParam(req.Params, "headers")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	wantCodes, err := util.OptIntSliceParam(req.Params, "status_codes")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if len(wantCodes) == 0 {
		wantCodes = []int64{http.StatusOK}
	}
	timeoutStr, err := util.OptStringParam(req.Params, "timeout")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	timeout := defaultTimeout
	if timeoutStr != "" {
		timeout, err = parseTimeout(timeoutStr)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
	}
	allowPrivate, err := util.OptBoolParam(req.Params, "allow_private")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	insecureSkipVerify, err := util.OptBoolParam(req.Params, "insecure_skip_verify")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// Клиент строится per-call под фактические opt-out-флаги задачи (три bool =
	// 8 комбинаций; пред-собранные инстанции не масштабируются). Три контура
	// ортогональны: allow_http не открывает SSRF (dial-guard живёт отдельно).
	doer := m.NewClient(util.HTTPClientOpts{
		AllowPrivate:       allowPrivate,
		InsecureSkipVerify: insecureSkipVerify,
		AllowHTTPRedirect:  allowHTTP,
	})

	status, body, truncated, elapsed, derr := m.do(stream.Context(), doer, method, rawURL, headers, timeout)
	if derr != nil {
		return util.SendFailed(stream, derr.Error())
	}

	out := buildOutput(status, body, truncated, elapsed, headers)
	if w := util.GuardWarnings(util.WarnHost(rawURL), util.HTTPClientOpts{
		AllowPrivate:       allowPrivate,
		InsecureSkipVerify: insecureSkipVerify,
		AllowHTTPRedirect:  allowHTTP,
	}); len(w) > 0 {
		out["warnings"] = util.StringsToAny(w)
	}

	// status вне ожидаемого набора → failed (явный контракт), но output
	// прикладываем: фактический status/body нужны для диагностики. Тело уже
	// санитизировано (do → sanitizeBody), поэтому structpb.NewStruct не упадёт
	// на не-UTF8; если всё же упадёт — не теряем диагностику молча, а пишем
	// причину в message (раньше output просто пропадал → потеря данных).
	if !containsCode(wantCodes, status) {
		ev := &pluginv1.ApplyEvent{
			Failed:  true,
			Message: fmt.Sprintf("probe %s %s: status %d not in expected %v", method, rawURL, status, wantCodes),
		}
		if s, serr := structpb.NewStruct(out); serr == nil {
			ev.Output = s
		} else {
			ev.Message += fmt.Sprintf(" (output serialization failed: %v)", serr)
		}
		return stream.Send(ev)
	}

	// changed=false конструктивно — read-probe не меняет состояние хоста.
	return util.SendFinal(stream, false, out)
}

// do выполняет один read-only HTTP-запрос. HEAD не читает тело. Тело GET
// читается с cap maxBodyBytes (защита от OOM): сверх лимита поток отбрасывается,
// truncated=true. Возвращает status, тело (для GET), флаг усечения, длительность.
//
// headers применяются к запросу, но НИКОГДА не логируются и не возвращаются
// (sensitive-by-construction, [ADR-010] §7.4).
func (m *Module) do(
	ctx context.Context, doer util.HTTPDoer, method, rawURL string, headers map[string]string, timeout time.Duration,
) (status int, body string, truncated bool, elapsed time.Duration, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, method, rawURL, nil)
	if err != nil {
		return 0, "", false, 0, fmt.Errorf("build request for %s: %v", rawURL, err)
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := doer.Do(httpReq)
	if err != nil {
		return 0, "", false, 0, fmt.Errorf("probe %s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if method == http.MethodHead {
		return resp.StatusCode, "", false, time.Since(start), nil
	}

	// Читаем не больше maxBodyBytes+1, чтобы отличить «ровно лимит» от «больше».
	buf, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	elapsed = time.Since(start)
	if rerr != nil {
		return 0, "", false, 0, fmt.Errorf("read body %s: %v", rawURL, rerr)
	}
	if len(buf) > maxBodyBytes {
		// Усечение по жёсткому байтовому cap может разрезать многобайтную
		// руну на границе — откатываем хвост до последней ПОЛНОЙ руны, чтобы
		// частичная руна не попала в output (structpb отвергает invalid UTF-8).
		return resp.StatusCode, sanitizeBody(trimPartialRune(buf[:maxBodyBytes])), true, elapsed, nil
	}
	return resp.StatusCode, sanitizeBody(buf), false, elapsed, nil
}

// trimPartialRune убирает с конца среза хвост, образующий неполную (разрезанную
// на границе cap) UTF-8-руну. Полное и валидное тело возвращается без изменений;
// если последний байт — оборванная многобайтная руна, она отбрасывается.
func trimPartialRune(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	r, size := utf8.DecodeLastRune(b)
	if r == utf8.RuneError && size <= 1 {
		// Последняя руна невалидна/обрезана. Если это разрезанный многобайтный
		// префикс (старший бит выставлен) — отбрасываем его, иначе это просто
		// одиночный битый байт, который позже починит sanitizeBody.
		if b[len(b)-1] >= 0x80 {
			return b[:len(b)-1]
		}
	}
	return b
}

// sanitizeBody приводит тело ответа к валидному UTF-8: probe — read-only HTTP,
// тело может оказаться бинарным или содержать битые байты, а structpb (output в
// register) принимает только валидный UTF-8. Битые последовательности заменяются
// на U+FFFD, чтобы probe возвращал чистый результат, а не ронял Apply gRPC-ошибкой.
func sanitizeBody(b []byte) string {
	return strings.ToValidUTF8(string(b), "�")
}

// buildOutput собирает register-output probe.
//
// Маскинг тела (ОГРАНИЧЕНИЕ — читать перед использованием):
// тело отдаётся как есть, sensitive-целиком оно НЕ считается — health-эндпоинт
// штатно возвращает `{"status":"ok"}`, ради этого probe и нужен. Из тела
// маскируются только vault-ref-подстроки (`vault:…` — маркер секрета проекта,
// его утечка в register/логи/OTel реальна), включая случай, когда vault-ref —
// не префикс, а значение внутри JSON (`{"token":"vault:secret/x"}`). Произвольные
// plaintext-секреты (например `password: hunter2`) НЕ маскируются: тело
// semi-trusted (ответ о здоровье сервиса), и оператор не должен класть в
// probe-эндпоинт то, что не должно светиться.
//
// headers — sensitive-by-construction: в output отдаются только КЛЮЧИ запрошенных
// заголовков (значения исключены конструктивно, [ADR-010] §7.4).
func buildOutput(status int, body string, truncated bool, elapsed time.Duration, headers map[string]string) map[string]any {
	out := map[string]any{
		"status":     status,
		"body":       maskBody(body),
		"truncated":  truncated,
		"elapsed_ms": elapsed.Milliseconds(),
		"changed":    false,
	}
	if len(headers) > 0 {
		out["headers_keys"] = headerKeys(headers)
	}
	return out
}

// vaultRefRe матчит vault-ref как ПОДСТРОКУ тела, а не только префикс всей
// строки: `vault:` + последовательность непробельных байт без кавычек (граница
// ref-а в JSON/YAML/тексте). Покрывает и whole-string-ref (`vault:secret/x`), и
// ref внутри структуры (`{"token":"vault:secret/x"}`). Прочие секреты не ловятся
// сознательно — см. buildOutput.
var vaultRefRe = regexp.MustCompile(`vault:[^\s"']+`)

// maskedValue — placeholder vault-ref-а в теле. Совпадает с audit-маской, чтобы
// register/логи/OTel были консистентны (audit.MaskSecrets маскирует whole-string
// vault-ref тем же значением; здесь мы расширяем до substring внутри тела).
const maskedValue = "***MASKED***"

// maskBody маскирует vault-ref-подстроки в теле ответа. Произвольные секреты не
// трогает — ограничение задокументировано в buildOutput.
func maskBody(body string) string {
	return vaultRefRe.ReplaceAllString(body, maskedValue)
}

// headerKeys возвращает отсортированный список ключей запрошенных заголовков
// (детерминизм; значения НЕ включаются — sensitive-by-construction).
// Тип []any, а не []string: structpb.NewStruct (SendFinal) принимает только
// []any в качестве list-значения.
func headerKeys(headers map[string]string) []any {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

// containsCode — линейный поиск (status_codes — короткий список, типично 1–3).
func containsCode(codes []int64, status int) bool {
	for _, c := range codes {
		if c == int64(status) {
			return true
		}
	}
	return false
}
