package scenario

// ★ Secret-critical guard tests for preparing params before persisting to
// apply_run_plan (maskRunPlanParams, NIM-37 S1b): seal-aware value masking
// (the same mechanism as status_details/error_summary), transport-key
// filtering, no_log suppression. A plaintext secret leak into
// apply_run_plan.params is caught here, before the DB.

import (
	"bytes"
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

func mustParamsStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func decodeRunPlanParams(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if raw == nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal params %s: %v", raw, err)
	}
	return m
}

// TestMaskRunPlanParams_PlainKept — regular (non-secret) params are kept as-is.
func TestMaskRunPlanParams_PlainKept(t *testing.T) {
	task := &render.RenderedTask{
		Name:   "install",
		Module: "core.pkg.installed",
		Params: mustParamsStruct(t, map[string]any{"name": "redis", "state": "present"}),
	}
	got := decodeRunPlanParams(t, maskRunPlanParams(task, nil))
	if got["name"] != "redis" || got["state"] != "present" {
		t.Errorf("params = %v, want {name:redis, state:present}", got)
	}
}

// TestMaskRunPlanParams_SealedSecretMasked ★ — a value on a run's sealed
// path is masked (no plaintext reaches apply_run_plan). Same seal-aware
// mechanism (SealOpts.Sealed) applied to status_details/error_summary.
func TestMaskRunPlanParams_SealedSecretMasked(t *testing.T) {
	const secret = "s3cr3t-pw-value"
	task := &render.RenderedTask{
		Name:   "set acl",
		Module: "core.exec.run",
		Params: mustParamsStruct(t, map[string]any{"command": "redis-cli", "password": secret}),
	}
	// password is marked sealed at the render phase (CEL read a secret source).
	raw := maskRunPlanParams(task, map[string]bool{"password": true})

	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("★ secret %q leaked into params plaintext: %s", secret, raw)
	}
	got := decodeRunPlanParams(t, raw)
	if got["password"] != "***MASKED***" {
		t.Errorf("password = %v, want ***MASKED***", got["password"])
	}
	if got["command"] != "redis-cli" {
		t.Errorf("command = %v, want redis-cli (not a secret - kept)", got["command"])
	}
}

// TestMaskRunPlanParams_VaultRefAndSensitiveKey ★ — a vault-ref value and a
// sensitive-by-name key are masked even WITHOUT the sealed set (vault +
// regex-last-resort layers): a second barrier for when seal provenance
// didn't mark the cell.
func TestMaskRunPlanParams_VaultRefAndSensitiveKey(t *testing.T) {
	task := &render.RenderedTask{
		Module: "core.exec.run",
		Params: mustParamsStruct(t, map[string]any{
			"ref":   "vault:secret/data/db#pw",
			"token": "raw-bearer-token",
			"host":  "10.0.0.1",
		}),
	}
	got := decodeRunPlanParams(t, maskRunPlanParams(task, nil))
	if got["ref"] != "***MASKED***" {
		t.Errorf("ref (vault-ref) = %v, want masked", got["ref"])
	}
	if got["token"] != "***MASKED***" {
		t.Errorf("token (sensitive key) = %v, want masked", got["token"])
	}
	if got["host"] != "10.0.0.1" {
		t.Errorf("host = %v, want 10.0.0.1 (not a secret - kept)", got["host"])
	}
}

// TestMaskRunPlanParams_TransportFiltered — core.file.rendered's transport
// keys (template_content/render_context) are dropped (not operator-facing
// input), operator keys are kept.
func TestMaskRunPlanParams_TransportFiltered(t *testing.T) {
	task := &render.RenderedTask{
		Module: "core.file.rendered",
		Params: mustParamsStruct(t, map[string]any{
			"path":             "/etc/app.conf",
			"template_content": "raw {{ .vars.x }}",
			"render_context":   map[string]any{"vars": map[string]any{"x": float64(1)}},
		}),
	}
	got := decodeRunPlanParams(t, maskRunPlanParams(task, nil))
	if _, ok := got["template_content"]; ok {
		t.Errorf("template_content not filtered out: %v", got)
	}
	if _, ok := got["render_context"]; ok {
		t.Errorf("render_context not filtered out: %v", got)
	}
	if got["path"] != "/etc/app.conf" {
		t.Errorf("path = %v, want /etc/app.conf (kept)", got["path"])
	}
}

// TestMaskRunPlanParams_NoLogNil — a no_log task → nil (params aren't
// stored, symmetric with register_data suppression).
func TestMaskRunPlanParams_NoLogNil(t *testing.T) {
	task := &render.RenderedTask{
		NoLog:  true,
		Module: "core.exec.run",
		Params: mustParamsStruct(t, map[string]any{"password": "x"}),
	}
	if raw := maskRunPlanParams(task, nil); raw != nil {
		t.Errorf("no_log params = %s, want nil", raw)
	}
}

// TestMaskRunPlanParams_NilAndEmptyRemainder — nil Params, and
// "transport-only" (empty remainder after the filter) → nil (jsonb NULL,
// omitempty on the wire).
func TestMaskRunPlanParams_NilAndEmptyRemainder(t *testing.T) {
	if raw := maskRunPlanParams(&render.RenderedTask{Module: "core.exec.run"}, nil); raw != nil {
		t.Errorf("nil Params → %s, want nil", raw)
	}
	only := &render.RenderedTask{
		Module: "core.file.rendered",
		Params: mustParamsStruct(t, map[string]any{"template_content": "x"}),
	}
	if raw := maskRunPlanParams(only, nil); raw != nil {
		t.Errorf("transport-only → %s, want nil (empty remainder)", raw)
	}
}

// TestMaskRunPlanParams_NestedSealedMasked ★ — a sealed path inside a nested
// structure (acl[].password) is masked via the generalized idx form,
// neighboring values stay intact.
func TestMaskRunPlanParams_NestedSealedMasked(t *testing.T) {
	const secret = "nested-secret"
	task := &render.RenderedTask{
		Module: "core.exec.run",
		Params: mustParamsStruct(t, map[string]any{
			"acl": []any{
				map[string]any{"user": "alice", "password": secret},
			},
		}),
	}
	raw := maskRunPlanParams(task, map[string]bool{"acl[].password": true})
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("★ nested secret leaked plaintext: %s", raw)
	}
	got := decodeRunPlanParams(t, raw)
	acl, ok := got["acl"].([]any)
	if !ok || len(acl) != 1 {
		t.Fatalf("acl = %v, want a slice of 1 element", got["acl"])
	}
	row, _ := acl[0].(map[string]any)
	if row["password"] != "***MASKED***" {
		t.Errorf("acl[0].password = %v, want ***MASKED***", row["password"])
	}
	if row["user"] != "alice" {
		t.Errorf("acl[0].user = %v, want alice (not a secret - kept)", row["user"])
	}
}
