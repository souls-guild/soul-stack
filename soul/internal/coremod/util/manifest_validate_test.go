package util_test

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func validateReq(t *testing.T, state string, params map[string]any) *pluginv1.ValidateRequest {
	t.Helper()
	req := &pluginv1.ValidateRequest{State: state}
	if params != nil {
		s, err := structpb.NewStruct(params)
		if err != nil {
			t.Fatalf("structpb.NewStruct: %v", err)
		}
		req.Params = s
	}
	return req
}

// Валидный state со всеми required-параметрами → пустой список ошибок.
// core.file.present требует path.
func TestValidateAgainstManifest_OK(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", validateReq(t, "present", map[string]any{
		"path":    "/etc/x",
		"content": "y",
	}))
	if len(errs) != 0 {
		t.Fatalf("errs=%v want none", errs)
	}
}

// Отсутствует required path → ровно одна ошибка про missing path.
func TestValidateAgainstManifest_MissingRequired(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", validateReq(t, "present", map[string]any{
		"content": "y",
	}))
	if len(errs) != 1 {
		t.Fatalf("errs=%v want 1", errs)
	}
	if !strings.Contains(errs[0], "path") || !strings.Contains(errs[0], "missing") {
		t.Fatalf("err=%q want про path missing", errs[0])
	}
}

// core.file.rendered требует path И template — оба отсутствуют → две ошибки в
// отсортированном порядке (path до template).
func TestValidateAgainstManifest_MultipleMissingSorted(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", validateReq(t, "rendered", nil))
	if len(errs) != 2 {
		t.Fatalf("errs=%v want 2", errs)
	}
	if !strings.Contains(errs[0], "path") {
		t.Fatalf("errs[0]=%q want path first (sorted)", errs[0])
	}
	if !strings.Contains(errs[1], "template") {
		t.Fatalf("errs[1]=%q want template second (sorted)", errs[1])
	}
}

// Неизвестный state → ошибка с перечнем допустимых.
func TestValidateAgainstManifest_UnknownState(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", validateReq(t, "no-such-state", nil))
	if len(errs) != 1 {
		t.Fatalf("errs=%v want 1", errs)
	}
	if !strings.Contains(errs[0], "unknown state") {
		t.Fatalf("err=%q want про unknown state", errs[0])
	}
	// Список допустимых упоминает существующие states.
	if !strings.Contains(errs[0], "present") || !strings.Contains(errs[0], "absent") {
		t.Fatalf("err=%q want перечень states", errs[0])
	}
}

// Неизвестный core-модуль → internal-диагностика, без паники.
func TestValidateAgainstManifest_UnknownModule(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.nonexistent", validateReq(t, "present", nil))
	if len(errs) != 1 || !strings.Contains(errs[0], "no manifest") {
		t.Fatalf("errs=%v want internal no manifest", errs)
	}
}

// Контракт paramPresent: явный null (Value с NullValue-Kind) трактуется как
// ОТСУТСТВИЕ — симметрично Apply-time getter-ам (Opt*Param в params.go).
// required-параметр со значением null обязан давать ошибку наличия уже на
// этапе manifest-валидации (lint), а не падать позже в runtime.
func TestValidateAgainstManifest_NullParamCountsAsAbsent(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", validateReq(t, "present", map[string]any{
		"path": nil,
	}))
	if len(errs) != 1 {
		t.Fatalf("errs=%v want 1 (null required-param = отсутствует)", errs)
	}
	if !strings.Contains(errs[0], "path") || !strings.Contains(errs[0], "missing") {
		t.Fatalf("err=%q want про path missing", errs[0])
	}
}

// Optional-параметр со значением null — НЕ ошибка: его отсутствие допустимо.
// path тут задан (required присутствует), content=null — опциональный.
func TestValidateAgainstManifest_OptionalNullParamOK(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", validateReq(t, "present", map[string]any{
		"path":    "/etc/x",
		"content": nil,
	}))
	if len(errs) != 0 {
		t.Fatalf("errs=%v want none (null у optional-параметра допустим)", errs)
	}
}

// nil Params для state с required → required-параметр считается отсутствующим.
func TestValidateAgainstManifest_NilParams(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", &pluginv1.ValidateRequest{State: "present"})
	if len(errs) != 1 || !strings.Contains(errs[0], "path") {
		t.Fatalf("errs=%v want path missing on nil params", errs)
	}
}
