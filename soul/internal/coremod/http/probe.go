package http

import (
	"bytes"
	"context"
	"errors"
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
	method, err := normalizedProbeMethod(req.Params)
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

	status, body, truncated, elapsed, derr := m.do(
		stream.Context(), doer, "probe", method, rawURL, headers, nil, "", timeout,
	)
	if derr != nil {
		return util.SendFailed(stream, derr.Error())
	}

	out := buildOutput(status, body, truncated, elapsed, headers, "", false)
	if w := util.GuardWarnings(util.WarnHost(rawURL), util.HTTPClientOpts{
		AllowPrivate:       allowPrivate,
		InsecureSkipVerify: insecureSkipVerify,
		AllowHTTPRedirect:  allowHTTP,
	}); len(w) > 0 {
		out["warnings"] = util.StringsToAny(redactHeaderValuesInStrings(w, headers, ""))
	}

	// status outside the expected set → failed (explicit contract), but we
	// still attach output: the actual status/body are needed for diagnosis.
	// The body is already sanitized (do → sanitizeBody), so structpb.NewStruct
	// shouldn't fail on non-UTF8; if it still does, don't lose the diagnostic
	// silently — write the reason into message (previously output just
	// vanished → data loss).
	if !containsCode(wantCodes, status) {
		ev := &pluginv1.ApplyEvent{
			Failed: true,
			Message: redactHeaderValues(
				fmt.Sprintf("probe %s %s: status %d not in expected %v", method, rawURL, status, wantCodes),
				headers,
				"",
			),
		}
		if s, serr := structpb.NewStruct(out); serr == nil {
			ev.Output = s
		} else {
			// Same message, second assembly site: it must pass through the
			// same redaction as the first one, or the fallback branch becomes
			// an unmasked twin of a masked path.
			ev.Message += redactHeaderValues(
				fmt.Sprintf(" (output serialization failed: %v)", serr), headers, "",
			)
		}
		return stream.Send(ev)
	}

	// changed=false by construction — a read-probe never changes host state.
	return util.SendFinal(stream, false, out)
}

// do performs exactly one HTTPDoer.Do invocation. It is shared by probe and
// request so their timeout, response cap, UTF-8 handling and transport
// diagnostics cannot drift. A probe's production client may follow guarded
// redirects for backward compatibility; request builds its client with
// DisableRedirects, making that one Do invocation exactly one wire request.
// HEAD doesn't read a response body; all other methods read at most
// maxBodyBytes+1 (OOM protection).
//
// headers are applied to the request but NEVER logged or returned
// (sensitive-by-construction, [ADR-010] §7.4).
func (m *Module) do(
	ctx context.Context,
	doer util.HTTPDoer,
	verb, method, rawURL string,
	headers map[string]string,
	requestBody []byte,
	contentType string,
	timeout time.Duration,
) (status int, body string, truncated bool, elapsed time.Duration, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if requestBody != nil {
		bodyReader = bytes.NewReader(requestBody)
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, method, rawURL, bodyReader)
	if err != nil {
		return 0, "", false, 0, errors.New(redactHeaderValues(
			fmt.Sprintf("build request for %s: %v", rawURL, err), headers, contentType,
		))
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	// The dedicated param wins over headers[Content-Type], making the
	// author-facing source of this standard header deterministic.
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}

	start := time.Now()
	resp, err := doer.Do(httpReq)
	if err != nil {
		return 0, "", false, 0, errors.New(redactHeaderValues(
			fmt.Sprintf("%s %s %s: %v", verb, method, rawURL, err), headers, contentType,
		))
	}
	defer func() { _ = resp.Body.Close() }()

	if method == http.MethodHead {
		return resp.StatusCode, "", false, time.Since(start), nil
	}

	// Read at most maxBodyBytes+1, to distinguish "exactly the limit" from "more".
	buf, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	elapsed = time.Since(start)
	if rerr != nil {
		return 0, "", false, 0, errors.New(redactHeaderValues(
			fmt.Sprintf("read body %s: %v", rawURL, rerr), headers, contentType,
		))
	}
	values := sensitiveValues(headers, contentType)
	rawTruncated := len(buf) > maxBodyBytes
	if rawTruncated {
		// EVERY cut can strand a plaintext prefix of a sensitive value, and the
		// byte cap is not the only cut here: the roll-back to the last complete
		// rune (structpb rejects invalid UTF-8) cuts again right after it. Both
		// are repaired in one descent — see trimCutTail.
		//
		// It happens HERE, while the bytes are still raw. sanitizeBody below
		// rewrites whatever is left of a cut rune as U+FFFD, and afterwards a
		// fragment no longer looks like a prefix of anything.
		buf = trimCutTail(buf[:maxBodyBytes], values)
	}
	body = redactResponseBody(sanitizeBody(buf), headers, contentType)
	// The third cut. Masking can GROW the text — any value shorter than the
	// mask does — so capBody cuts again. Every WHOLE value is a mask by now,
	// but a PREFIX of one is not, and it can end exactly at the new boundary.
	body, outputTruncated := capBody(body)
	if outputTruncated {
		body = string(trimCutTail([]byte(body), values))
	}
	truncated = rawTruncated || outputTruncated
	return resp.StatusCode, body, truncated, elapsed, nil
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

// buildOutput assembles probe/request register output.
//
// Body masking (LIMITATION — read before relying on this):
// the body is NOT treated as sensitive wholesale — a
// health endpoint typically returns `{"status":"ok"}`, which is the whole
// point of probe. Vault-ref substrings and every effective request-header
// value are masked in the body
// (`vault:…` — the project's secret marker; its leak into register/logs/OTel
// is a real risk), including when the vault-ref isn't the whole value but
// embedded in JSON (`{"token":"vault:secret/x"}`). Arbitrary plaintext
// secrets (e.g. `password: hunter2`) are NOT masked: the body is
// semi-trusted (a service health response), and the operator shouldn't put
// anything sensitive behind a probe endpoint.
//
// headers are sensitive-by-construction: output only carries the KEYS of
// requested headers (values are excluded by construction, [ADR-010] §7.4).
func buildOutput(
	status int,
	body string,
	truncated bool,
	elapsed time.Duration,
	headers map[string]string,
	contentType string,
	changed bool,
) map[string]any {
	out := map[string]any{
		"status":     status,
		"body":       body,
		"truncated":  truncated,
		"elapsed_ms": elapsed.Milliseconds(),
		"changed":    changed,
	}
	if len(headers) > 0 || contentType != "" {
		out["headers_keys"] = headerKeys(headers, contentType)
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

// redactResponseBody applies all body redaction before the final output cap.
// Header values are treated as sensitive regardless of their key: a server
// that echoes X-Consul-Token (or any other request header) must not be able to
// feed that value back into register/log/audit via output.body.
//
// Header values go FIRST. A value can itself contain a vault-ref — an
// unresolved `Authorization: Bearer vault:secret/x` reaches the wire as written
// — and masking the ref first rewrites the MIDDLE of that value, leaving the
// whole-value matcher nothing to match and the value's leading bytes
// (`Bearer `) in plaintext. The reverse order has no such interaction: what
// header redaction leaves behind is the mask, which contains no `vault:`.
func redactResponseBody(body string, headers map[string]string, contentType string) string {
	return maskBody(redactHeaderValues(body, headers, contentType))
}

// sensitiveValues returns the effective non-empty request-header values (plus
// the dedicated content_type param, which becomes a header on the wire),
// deduplicated. Order is not load-bearing — the masker unions coverage across
// values and cutFragments takes a maximum, both order-independent — but map
// iteration is random, so the slice is sorted (longest first, then lexically)
// to keep behaviour reproducible run to run.
//
// Values arrive from structpb, i.e. from a proto3 string, and are therefore
// valid UTF-8 by the wire contract. cutFragments leans on that: a value whose
// first byte were a continuation byte could put a cut inside a rune.
func sensitiveValues(headers map[string]string, contentType string) []string {
	values := make([]string, 0, len(headers)+1)
	seen := make(map[string]struct{}, len(headers)+1)
	for _, value := range headers {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	if contentType != "" {
		if _, ok := seen[contentType]; !ok {
			values = append(values, contentType)
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) == len(values[j]) {
			return values[i] < values[j]
		}
		return len(values[i]) > len(values[j])
	})
	return values
}

// trimCutTail rolls a cut body back to a boundary that is clean on BOTH counts
// at once: it ends at a complete UTF-8 rune, and it does not end with a PROPER
// PREFIX of a sensitive value. A cut lands at a byte offset before redaction
// runs, so a value straddling it survives as a plaintext prefix that the
// whole-value masker cannot recognise. Only the tail can hold such a fragment:
// the body is never cut at the head.
//
// The two rules descend TOGETHER, because each one is itself a cut and can
// expose work for the other: rolling a partial rune back re-exposes a fragment
// that ended just before it, and dropping a fragment can leave a partial rune
// underneath. Descending until neither applies is the only stable answer.
//
// On the raw path it must run before the cut is tidied up. A prefix ending
// mid-rune is still a byte-exact prefix here; once sanitizeBody has turned that
// partial rune into U+FFFD, no comparison can find it again. Raw is also the
// only form in which the fragment is contiguous: a fragment long enough to
// contain a shorter sensitive value in full would be split in two by masking
// that value, and the remainder — a middle slice of the longer value, prefix of
// nothing — would then survive every subsequent check.
//
// The fragment table is computed ONCE and stays valid the whole way down:
// frag[cut], and whether body[:cut] ends mid-rune, depend only on body[:cut],
// which dropping a tail never changes. Recomputing per step would be quadratic
// in the BODY, and the body is precisely what the endpoint controls — an answer
// whose tail is one long run of continuation bytes forces a roll-back per byte,
// which measured ~15s of CPU per response at the 64 KiB cap.
//
// The dropped bytes are not replaced by the mask: the body is already reported
// with truncated=true, so losing them is inside the response contract, whereas
// appending a mask could push the text back over the cap.
func trimCutTail(body []byte, values []string) []byte {
	frag := cutFragments(body, values)
	cut := len(body)
	for cut > 0 {
		if n := frag[cut]; n > 0 {
			cut -= n
			continue
		}
		trimmed := trimPartialRune(body[:cut])
		if len(trimmed) == cut {
			break
		}
		cut = len(trimmed)
	}
	return body[:cut]
}

// cutFragments returns, for every offset in body, the length of the longest
// PROPER PREFIX of a sensitive value ending there — with one exception: a value
// ending COMPLETE at that offset contributes nothing. A complete value is not a
// cut fragment; whole-value masking covers it, and stripping it as if it were a
// fragment is actively harmful. A value with a period ("secretsecret") ends with
// its own proper prefix, so a length-only rule eats its second half and leaves
// the first in plaintext — destroying a value the masker would have covered.
//
// "The masker covers it" is a guarantee here, not an assumption, and only
// because the masker marks covered BYTES. A value with a period is exactly the
// shape that produces OVERLAPPING occurrences at a cut, and byte coverage
// unions them into a single masked run. While the masker instead consumed
// non-overlapping matches, this exception was a hole: `secretsecretsecret` left
// at the cap kept frag=0 here, and the masker's cursor then skipped straight
// past the third occurrence, leaking half the value in plaintext.
//
// Every offset is computed, not just the last one, because the descent above
// looks up each new cut in turn. The obvious alternative — rescan the tail after
// every cut — is quadratic in the value's length, and both inputs are within
// reach of the endpoint being called: it receives the header value itself, so it
// can answer with the body that maximises the scan (a 60 KiB value cost ~1.4s of
// CPU per response). This walks a KMP automaton per value in one pass instead,
// linear in body plus value.
func cutFragments(body []byte, values []string) []int {
	frag := make([]int, len(body)+1)
	for _, value := range values {
		if len(value) < 2 {
			continue // no proper prefix to strand
		}
		fail := kmpFailure(value)
		k := 0
		for i := 0; i < len(body); i++ {
			for k > 0 && body[i] != value[k] {
				k = fail[k-1]
			}
			if body[i] == value[k] {
				k++
			}
			n := k
			if k == len(value) {
				n, k = 0, fail[k-1]
			}
			if n > frag[i+1] {
				frag[i+1] = n
			}
		}
	}
	return frag
}

// kmpFailure returns the Knuth-Morris-Pratt failure function of pat: fail[i] is
// the length of the longest proper prefix of pat[:i+1] that is also its suffix.
func kmpFailure(pat string) []int {
	fail := make([]int, len(pat))
	k := 0
	for i := 1; i < len(pat); i++ {
		for k > 0 && pat[i] != pat[k] {
			k = fail[k-1]
		}
		if pat[i] == pat[k] {
			k++
		}
		fail[i] = k
	}
	return fail
}

// redactHeaderValues masks every effective non-empty request-header value in
// arbitrary text.
func redactHeaderValues(text string, headers map[string]string, contentType string) string {
	return maskSensitiveValues(text, sensitiveValues(headers, contentType))
}

// maskSensitiveValues replaces the sensitive values in text with the mask,
// collapsing each maximal run of covered bytes into ONE mask.
//
// It deliberately is not strings.Replacer, which is what this used to be. A
// Replacer walks the text once and skips past every match it consumes, so by
// its own contract it performs "no overlapping matches": an occurrence that
// STARTS inside an already-replaced one is never even considered, and the part
// of it that sticks out stays in output.body verbatim. That is up to
// len(value)-1 plaintext bytes of a credential, and it needs no truncation to
// happen — any value with a border ("secretsecret", "aaaa") overlaps ITSELF, so
// a body echoing it twice back to back is enough, and a truncated body produces
// that shape on its own when the cap lands inside a repeat.
//
// Covering bytes removes the class rather than an instance of it: overlapping
// occurrences union into one run and a run is masked whole, so no remainder can
// exist by construction. Adjacent occurrences merge into a single mask too — a
// deliberate side effect, since how many values sat there is not worth
// reporting either.
//
// Masks are never rescanned: coverage is computed over the input and the output
// is assembled once, so a value that literally equals the mask cannot recurse.
func maskSensitiveValues(text string, values []string) string {
	covered := coverSensitiveValues(text, values)
	if covered == nil {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); {
		j := i
		for j < len(text) && covered[j] == covered[i] {
			j++
		}
		if covered[i] {
			out.WriteString(maskedValue)
		} else {
			out.WriteString(text[i:j])
		}
		i = j
	}
	return out.String()
}

// coverSensitiveValues marks every byte of text belonging to at least one
// COMPLETE occurrence of a sensitive value, and returns nil when nothing
// matched at all (the overwhelmingly common case — the caller then hands back
// the original string untouched).
//
// Occurrences accumulate in a difference array instead of being written span by
// span. A value can match at nearly every offset — a run of one byte under a
// value made of that same byte — and painting each match byte by byte would be
// quadratic in the value's length, with both the body and the value inside the
// reach of the endpoint being called.
func coverSensitiveValues(text string, values []string) []bool {
	if len(text) == 0 || len(values) == 0 {
		return nil
	}
	delta := make([]int, len(text)+1)
	matched := false
	for _, value := range values {
		if value == "" {
			continue
		}
		fail := kmpFailure(value)
		k := 0
		for i := 0; i < len(text); i++ {
			for k > 0 && text[i] != value[k] {
				k = fail[k-1]
			}
			if text[i] == value[k] {
				k++
			}
			if k == len(value) {
				delta[i-len(value)+1]++
				delta[i+1]--
				matched = true
				// Keep the automaton running rather than restarting it: the very
				// next offset may open another occurrence overlapping this one.
				k = fail[k-1]
			}
		}
	}
	if !matched {
		return nil
	}
	covered := make([]bool, len(text))
	depth := 0
	for i := range covered {
		depth += delta[i]
		covered[i] = depth > 0
	}
	return covered
}

func redactHeaderValuesInStrings(in []string, headers map[string]string, contentType string) []string {
	out := make([]string, len(in))
	for i, value := range in {
		out[i] = redactHeaderValues(value, headers, contentType)
	}
	return out
}

// capBody enforces the response contract on the FINAL text after UTF-8
// sanitation and secret redaction, both of which can expand bytes. Input is
// valid UTF-8; rolling back until the prefix is valid avoids a partial rune.
func capBody(body string) (string, bool) {
	if len(body) <= maxBodyBytes {
		return body, false
	}
	prefix := []byte(body)[:maxBodyBytes]
	for len(prefix) > 0 && !utf8.Valid(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return string(prefix), true
}

// headerKeys returns a sorted list of requested header keys (deterministic;
// values are NOT included — sensitive-by-construction).
// Type []any, not []string: structpb.NewStruct (SendFinal) only accepts
// []any as a list value.
func headerKeys(headers map[string]string, contentType string) []any {
	keys := make([]string, 0, len(headers)+1)
	seen := make(map[string]struct{}, len(headers)+1)
	for k := range headers {
		canonical := http.CanonicalHeaderKey(k)
		if _, ok := seen[canonical]; !ok {
			seen[canonical] = struct{}{}
			// Preserve probe's historical output spelling for caller-supplied
			// keys; canonicalization is only for case-insensitive de-duplication.
			keys = append(keys, k)
		}
	}
	if contentType != "" {
		if _, ok := seen["Content-Type"]; !ok {
			keys = append(keys, "Content-Type")
		}
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
