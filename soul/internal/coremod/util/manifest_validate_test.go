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

// A valid state with all required params → empty error list.
// core.file.present requires path.
func TestValidateAgainstManifest_OK(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", validateReq(t, "present", map[string]any{
		"path":    "/etc/x",
		"content": "y",
	}))
	if len(errs) != 0 {
		t.Fatalf("errs=%v want none", errs)
	}
}

// Missing required path → exactly one error about missing path.
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

// core.file.rendered requires path AND template — both missing → two errors
// in sorted order (path before template).
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

// Unknown state → error listing the valid ones.
func TestValidateAgainstManifest_UnknownState(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", validateReq(t, "no-such-state", nil))
	if len(errs) != 1 {
		t.Fatalf("errs=%v want 1", errs)
	}
	if !strings.Contains(errs[0], "unknown state") {
		t.Fatalf("err=%q want про unknown state", errs[0])
	}
	// The valid-states list mentions the actual states.
	if !strings.Contains(errs[0], "present") || !strings.Contains(errs[0], "absent") {
		t.Fatalf("err=%q want перечень states", errs[0])
	}
}

// Unknown core module → internal diagnostic, no panic.
func TestValidateAgainstManifest_UnknownModule(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.nonexistent", validateReq(t, "present", nil))
	if len(errs) != 1 || !strings.Contains(errs[0], "no manifest") {
		t.Fatalf("errs=%v want internal no manifest", errs)
	}
}

// paramPresent contract: an explicit null (a Value with NullValue-Kind) is
// treated as ABSENT — symmetric with the Apply-time getters (Opt*Param in
// params.go). A required param set to null must fail presence at
// manifest-validation time (lint), not later at runtime.
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

// An optional param set to null is NOT an error: absence is allowed.
// path is set here (required present), content=null is the optional one.
func TestValidateAgainstManifest_OptionalNullParamOK(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", validateReq(t, "present", map[string]any{
		"path":    "/etc/x",
		"content": nil,
	}))
	if len(errs) != 0 {
		t.Fatalf("errs=%v want none (null у optional-параметра допустим)", errs)
	}
}

// nil Params for a state with required params → required param counts as absent.
func TestValidateAgainstManifest_NilParams(t *testing.T) {
	errs := util.ValidateAgainstManifest("core.file", &pluginv1.ValidateRequest{State: "present"})
	if len(errs) != 1 || !strings.Contains(errs[0], "path") {
		t.Fatalf("errs=%v want path missing on nil params", errs)
	}
}
