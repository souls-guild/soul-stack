package http

import (
	"fmt"
	"net/http"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// applyRequest implements verb `request`: exactly one explicit mutating HTTP
// call (POST/PUT/PATCH/DELETE). The module does not retry: retry/until belongs
// to the scenario so every extra mutation is visible in author intent.
//
// Success means the transport completed and status matched status_codes
// (default [200]); it reports changed=true. A mismatch reports failed with the
// same diagnostic output as probe, but changed=false because the task did not
// confirm a successful mutation.
func (m *Module) applyRequest(
	stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest,
) error {
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
	method, err := normalizedRequestMethod(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	headers, err := util.OptStringMapParam(req.Params, "headers")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	body, err := util.OptStringParam(req.Params, "body")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	contentType, err := util.OptStringParam(req.Params, "content_type")
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

	clientOpts := util.HTTPClientOpts{
		AllowPrivate:       allowPrivate,
		InsecureSkipVerify: insecureSkipVerify,
		AllowHTTPRedirect:  allowHTTP,
		DisableRedirects:   true,
	}
	doer := m.NewClient(clientOpts)

	status, responseBody, truncated, elapsed, derr := m.do(
		stream.Context(), doer, "request", method, rawURL, headers, []byte(body), contentType, timeout,
	)
	if derr != nil {
		return util.SendFailed(stream, derr.Error())
	}

	succeeded := containsCode(wantCodes, status)
	out := buildOutput(status, responseBody, truncated, elapsed, headers, contentType, succeeded)
	// request's response contract always includes the effective header-name
	// list, including the empty list. Probe keeps its historical omission when
	// no headers were supplied.
	if _, ok := out["headers_keys"]; !ok {
		out["headers_keys"] = []any{}
	}
	if warnings := util.GuardWarnings(util.WarnHost(rawURL), clientOpts); len(warnings) > 0 {
		out["warnings"] = util.StringsToAny(redactHeaderValuesInStrings(warnings, headers, contentType))
	}

	if !succeeded {
		ev := &pluginv1.ApplyEvent{
			Failed: true,
			Message: redactHeaderValues(
				fmt.Sprintf("request %s %s: status %d not in expected %v", method, rawURL, status, wantCodes),
				headers,
				contentType,
			),
		}
		if output, serr := structpb.NewStruct(out); serr == nil {
			ev.Output = output
		} else {
			// Same message, second assembly site: it must pass through the
			// same redaction as the first one, or the fallback branch becomes
			// an unmasked twin of a masked path.
			ev.Message += redactHeaderValues(
				fmt.Sprintf(" (output serialization failed: %v)", serr), headers, contentType,
			)
		}
		return stream.Send(ev)
	}

	return util.SendFinal(stream, true, out)
}
