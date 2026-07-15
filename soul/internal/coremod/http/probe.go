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

// applyProbe implements verb `probe`: one GET/HEAD request to url, response
// goes to register. Host state never changes → changed=false always.
//
// Error contract:
//   - transport error (DNS/TLS/timeout/blocked downgrade redirect)
//     → failed (output is meaningless);
//   - status code outside status_codes (default [200]) → failed, but with
//     output: the operator needs the actual status/body for diagnosis.
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

	// The client is built per-call for the task's actual opt-out flags (three
	// bools = 8 combinations; pre-built instances don't scale). The three
	// controls are orthogonal: allow_http doesn't open up SSRF (the dial guard
	// lives separately).
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

	// status outside the expected set → failed (explicit contract), but we
	// still attach output: the actual status/body are needed for diagnosis.
	// The body is already sanitized (do → sanitizeBody), so structpb.NewStruct
	// shouldn't fail on non-UTF8; if it still does, don't lose the diagnostic
	// silently — write the reason into message (previously output just
	// vanished → data loss).
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

	// changed=false by construction — a read-probe never changes host state.
	return util.SendFinal(stream, false, out)
}

// do performs a single read-only HTTP request. HEAD doesn't read the body.
// GET's body is read with a maxBodyBytes cap (OOM protection): beyond the
// limit the stream is dropped, truncated=true. Returns status, body (for
// GET), truncation flag, duration.
//
// headers are applied to the request but NEVER logged or returned
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

	// Read at most maxBodyBytes+1, to distinguish "exactly the limit" from "more".
	buf, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	elapsed = time.Since(start)
	if rerr != nil {
		return 0, "", false, 0, fmt.Errorf("read body %s: %v", rawURL, rerr)
	}
	if len(buf) > maxBodyBytes {
		// Truncating at a hard byte cap can cut a multi-byte rune at the
		// boundary — trim back to the last COMPLETE rune so a partial rune
		// never reaches output (structpb rejects invalid UTF-8).
		return resp.StatusCode, sanitizeBody(trimPartialRune(buf[:maxBodyBytes])), true, elapsed, nil
	}
	return resp.StatusCode, sanitizeBody(buf), false, elapsed, nil
}

// trimPartialRune strips a trailing tail that forms an incomplete (cut at the
// cap boundary) UTF-8 rune. A full, valid body is returned unchanged; if the
// last byte is a truncated multi-byte rune, it's dropped.
func trimPartialRune(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	r, size := utf8.DecodeLastRune(b)
	if r == utf8.RuneError && size <= 1 {
		// The last rune is invalid/truncated. If it's a cut multi-byte prefix
		// (high bit set), drop it; otherwise it's just a single stray byte that
		// sanitizeBody will fix up later.
		if b[len(b)-1] >= 0x80 {
			return b[:len(b)-1]
		}
	}
	return b
}

// sanitizeBody coerces the response body to valid UTF-8: probe is a
// read-only HTTP call, the body may be binary or contain stray bytes, and
// structpb (output in register) only accepts valid UTF-8. Bad sequences are
// replaced with U+FFFD so probe returns a clean result instead of failing
// Apply with a gRPC error.
func sanitizeBody(b []byte) string {
	return strings.ToValidUTF8(string(b), "�")
}

// buildOutput assembles probe's register output.
//
// Body masking (LIMITATION — read before relying on this):
// the body is returned as-is; it's NOT treated as sensitive wholesale — a
// health endpoint typically returns `{"status":"ok"}`, which is the whole
// point of probe. Only vault-ref substrings are masked in the body
// (`vault:…` — the project's secret marker; its leak into register/logs/OTel
// is a real risk), including when the vault-ref isn't the whole value but
// embedded in JSON (`{"token":"vault:secret/x"}`). Arbitrary plaintext
// secrets (e.g. `password: hunter2`) are NOT masked: the body is
// semi-trusted (a service health response), and the operator shouldn't put
// anything sensitive behind a probe endpoint.
//
// headers are sensitive-by-construction: output only carries the KEYS of
// requested headers (values are excluded by construction, [ADR-010] §7.4).
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

// vaultRefRe matches a vault-ref as a SUBSTRING of the body, not just a
// whole-string prefix: `vault:` + a run of non-whitespace, non-quote bytes
// (the ref's boundary in JSON/YAML/text). Covers both a whole-string ref
// (`vault:secret/x`) and a ref embedded in a structure
// (`{"token":"vault:secret/x"}`). Other secrets are deliberately not caught
// — see buildOutput.
var vaultRefRe = regexp.MustCompile(`vault:[^\s"']+`)

// maskedValue is the vault-ref placeholder in the body. Matches the audit
// mask so register/logs/OTel stay consistent (audit.MaskSecrets masks a
// whole-string vault-ref with the same value; here we extend that to a
// substring within the body).
const maskedValue = "***MASKED***"

// maskBody masks vault-ref substrings in the response body. Doesn't touch
// arbitrary secrets — the limitation is documented in buildOutput.
func maskBody(body string) string {
	return vaultRefRe.ReplaceAllString(body, maskedValue)
}

// headerKeys returns a sorted list of requested header keys (deterministic;
// values are NOT included — sensitive-by-construction).
// Type []any, not []string: structpb.NewStruct (SendFinal) only accepts
// []any as a list value.
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

// containsCode does a linear search (status_codes is a short list, typically 1–3).
func containsCode(codes []int64, status int) bool {
	for _, c := range codes {
		if c == int64(status) {
			return true
		}
	}
	return false
}
