package scenario

// ★ Секрет-критичные guard-тесты подготовки params к персисту в apply_run_plan
// (maskRunPlanParams, NIM-37 S1b): seal-aware маскинг значений (тот же механизм,
// что status_details/error_summary), фильтр транспортных ключей, подавление на
// no_log. Утечка plaintext-секрета в apply_run_plan.params здесь ловится ДО БД.

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

// TestMaskRunPlanParams_PlainKept — обычные (не секретные) params сохраняются как есть.
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

// TestMaskRunPlanParams_SealedSecretMasked ★ — значение по sealed-пути прогона
// замаскировано (не уходит plaintext в apply_run_plan). Тот же seal-aware механизм
// (SealOpts.Sealed), что применяется к status_details/error_summary.
func TestMaskRunPlanParams_SealedSecretMasked(t *testing.T) {
	const secret = "s3cr3t-pw-value"
	task := &render.RenderedTask{
		Name:   "set acl",
		Module: "core.exec.run",
		Params: mustParamsStruct(t, map[string]any{"command": "redis-cli", "password": secret}),
	}
	// password помечен sealed на render-фазе (CEL читал secret-источник).
	raw := maskRunPlanParams(task, map[string]bool{"password": true})

	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("★ секрет %q утёк в params plaintext: %s", secret, raw)
	}
	got := decodeRunPlanParams(t, raw)
	if got["password"] != "***MASKED***" {
		t.Errorf("password = %v, want ***MASKED***", got["password"])
	}
	if got["command"] != "redis-cli" {
		t.Errorf("command = %v, want redis-cli (не секрет — сохранён)", got["command"])
	}
}

// TestMaskRunPlanParams_VaultRefAndSensitiveKey ★ — значение-vault-ref и sensitive-
// by-name ключ маскируются даже БЕЗ sealed-набора (слои vault + regex-last-resort):
// второй барьер на случай, если seal-провенанс не пометил ячейку.
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
		t.Errorf("host = %v, want 10.0.0.1 (не секрет — сохранён)", got["host"])
	}
}

// TestMaskRunPlanParams_TransportFiltered — транспортные ключи core.file.rendered
// (template_content/render_context) выкинуты (не operator-facing вход), operator-
// ключи сохранены.
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
		t.Errorf("template_content не отфильтрован: %v", got)
	}
	if _, ok := got["render_context"]; ok {
		t.Errorf("render_context не отфильтрован: %v", got)
	}
	if got["path"] != "/etc/app.conf" {
		t.Errorf("path = %v, want /etc/app.conf (сохранён)", got["path"])
	}
}

// TestMaskRunPlanParams_NoLogNil — no_log-задача → nil (params не хранятся,
// симметрия с подавлением register_data).
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

// TestMaskRunPlanParams_NilAndEmptyRemainder — nil Params, и «только транспорт»
// (пустой остаток после фильтра) → nil (jsonb NULL, omitempty на wire).
func TestMaskRunPlanParams_NilAndEmptyRemainder(t *testing.T) {
	if raw := maskRunPlanParams(&render.RenderedTask{Module: "core.exec.run"}, nil); raw != nil {
		t.Errorf("nil Params → %s, want nil", raw)
	}
	only := &render.RenderedTask{
		Module: "core.file.rendered",
		Params: mustParamsStruct(t, map[string]any{"template_content": "x"}),
	}
	if raw := maskRunPlanParams(only, nil); raw != nil {
		t.Errorf("только транспорт → %s, want nil (пустой остаток)", raw)
	}
}

// TestMaskRunPlanParams_NestedSealedMasked ★ — sealed-путь во вложенной структуре
// (acl[].password) маскируется по обобщённой idx-форме, соседние значения целы.
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
		t.Fatalf("★ вложенный секрет утёк plaintext: %s", raw)
	}
	got := decodeRunPlanParams(t, raw)
	acl, ok := got["acl"].([]any)
	if !ok || len(acl) != 1 {
		t.Fatalf("acl = %v, want срез из 1 элемента", got["acl"])
	}
	row, _ := acl[0].(map[string]any)
	if row["password"] != "***MASKED***" {
		t.Errorf("acl[0].password = %v, want ***MASKED***", row["password"])
	}
	if row["user"] != "alice" {
		t.Errorf("acl[0].user = %v, want alice (не секрет — сохранён)", row["user"])
	}
}
